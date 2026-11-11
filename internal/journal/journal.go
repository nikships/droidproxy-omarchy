package journal

import "time"

// End is how the relay stopped.
type End int

const (
	// endSettled: a successful client write delivered protocol completion,
	// HTTP framing completion, or a protocol error.
	endSettled End = iota
	// EndEOF: CLIProxyAPI closed the connection.
	EndEOF
	// EndBackendUnreachable: dialing CLIProxyAPI failed; the proxy sent 502.
	EndBackendUnreachable
	// EndSendFailed: writing the request to CLIProxyAPI failed.
	EndSendFailed
	// EndReadError: reading the response failed.
	EndReadError
	// EndClientWriteFailed: writing the response to the client failed.
	EndClientWriteFailed
	// EndCancelled: the relay stopped for any other reason.
	EndCancelled
)

// Journal creates observations for admitted requests while its setting is
// on and appends their records.
type Journal struct {
	enabled func() bool
	seats   *seatIndex
	writer  *recordWriter
}

// New returns a journal writing to dir and resolving seats in authDir.
// enabled is checked once per request.
func New(dir, authDir string, enabled func() bool) *Journal {
	return &Journal{
		enabled: enabled,
		seats:   newSeatIndex(authDir),
		writer:  newRecordWriter(dir, writerQueueSize),
	}
}

// AuthDirectoryChanged makes the next seat lookup rescan the auth directory.
func (j *Journal) AuthDirectoryChanged() {
	if j != nil {
		j.seats.invalidate()
	}
}

// Close writes queued records until the shutdown flush deadline and counts
// the rest as dropped.
func (j *Journal) Close() {
	if j != nil {
		j.writer.close()
	}
}

// Begin returns an observation for a POST to an admitted inference path, or
// nil when the journal is off or the request is not journaled. All
// Observation methods accept nil.
func (j *Journal) Begin(ctx RequestContext) *Observation {
	if j == nil || ctx.Method != "POST" || !admitted(stripQuery(ctx.ForwardedPath)) || !j.enabled() {
		return nil
	}
	return j.begin(ctx, time.Now())
}

func (j *Journal) begin(ctx RequestContext, now time.Time) *Observation {
	headers := parseHeaderLines(ctx.Head)
	body := strictObject([]byte(ctx.Body))

	o := &Observation{
		journal:  j,
		started:  now,
		endpoint: stripQuery(ctx.Path),
		origin:   "client",
	}
	forwarded := stripQuery(ctx.ForwardedPath)
	if forwarded != o.endpoint {
		o.forwarded = ptr(forwarded)
	}
	o.resp.envelope = envelopeFor(forwarded)
	if isPrimer(headers) {
		o.origin = "primer"
	}
	o.model = requestedModel(o.endpoint, body)
	model := ""
	if o.model != nil {
		model = *o.model
	}
	o.provider = deriveProvider(model, o.endpoint)
	o.session = clientSession(headers, body)
	return o
}

// Observation follows one relayed request. It is used from the relay
// goroutine only.
type Observation struct {
	journal *Journal

	started   time.Time
	firstBody time.Time
	finished  bool

	endpoint  string
	forwarded *string
	origin    string
	provider  string
	model     *string
	session   *string

	resp response

	executionID string
	authIndex   string
	binding     seatBinding
	bound       bool
}

// Delivered observes a buffer the client write accepted in full. readAt is
// when the upstream read that returned the buffer completed. The record is
// finalized here once the buffer delivers completion or a protocol error.
func (o *Observation) Delivered(p []byte, readAt time.Time) {
	if o == nil {
		return
	}
	if rec, ok := o.deliverAt(p, readAt, time.Now()); ok {
		o.journal.writer.enqueue(rec)
	}
}

// WriteFailed observes a buffer whose client write accepted only its first
// accepted bytes and then failed, and finalizes the record.
func (o *Observation) WriteFailed(p []byte, accepted int, readAt time.Time) {
	if o == nil {
		return
	}
	if rec, ok := o.writeFailedAt(p, accepted, readAt, time.Now()); ok {
		o.journal.writer.enqueue(rec)
	}
}

// Finish finalizes the observation once and queues its record.
func (o *Observation) Finish(end End) {
	if o == nil {
		return
	}
	if rec, ok := o.finishAt(end, time.Now()); ok {
		o.journal.writer.enqueue(rec)
	}
}

func (o *Observation) deliverAt(p []byte, readAt, writeAt time.Time) (Record, bool) {
	if o.finished {
		return Record{}, false
	}
	o.observe(p, readAt, writeAt)
	if o.resp.settled() {
		return o.finishAt(endSettled, writeAt)
	}
	return Record{}, false
}

func (o *Observation) writeFailedAt(p []byte, accepted int, readAt, now time.Time) (Record, bool) {
	if o.finished {
		return Record{}, false
	}
	accepted = min(max(accepted, 0), len(p))
	o.observe(p[:accepted], readAt, now)
	if o.resp.learnHead(p[accepted:]) {
		o.noteTrace(now)
	}
	return o.finishAt(EndClientWriteFailed, now)
}

func (o *Observation) observe(p []byte, readAt, now time.Time) {
	traceNow, bodyNow := o.resp.feed(p)
	if traceNow {
		o.noteTrace(now)
	}
	if bodyNow {
		o.firstBody = readAt
	}
}

func (o *Observation) noteTrace(now time.Time) {
	executionID, index := parseTrace(o.resp.trace)
	o.executionID = executionID
	if index != "" {
		o.authIndex = index
		o.binding, o.bound = o.journal.seats.bind(index, now)
	}
}

func (o *Observation) finishAt(end End, now time.Time) (Record, bool) {
	if o.finished {
		return Record{}, false
	}
	o.finished = true
	r := &o.resp

	var outcome string
	switch {
	case end == EndBackendUnreachable:
		outcome = outcomeBackendUnreachable
		r.status = 502
	case r.malformed:
		outcome = outcomeUpstreamError
		r.tokens = nil
	case end == EndClientWriteFailed:
		outcome = outcomeClientDisconnected
	case r.completed:
		outcome = outcomeComplete
	case r.protocolError:
		outcome = outcomeUpstreamError
	case end == EndEOF:
		outcome = outcomeUpstreamClosed
		// EOF ends a close-delimited body. Only a non-stream body is
		// complete then; an SSE stream needed delivered protocol completion.
		if r.headDone && r.framing == framingClose && !r.sse {
			r.framingEnded()
			outcome = outcomeComplete
		}
	default:
		outcome = outcomeUpstreamError
	}

	rec := Record{
		V:                 1,
		ExecutionID:       o.executionID,
		StartedAt:         formatTimestamp(o.started),
		EndedAt:           formatTimestamp(now),
		LatencyMS:         millis(now.Sub(o.started)),
		Origin:            o.origin,
		Endpoint:          o.endpoint,
		ForwardedEndpoint: o.forwarded,
		Provider:          o.provider,
		RequestedModel:    o.model,
		Outcome:           outcome,
		ClientSession:     o.session,
		Tokens:            r.tokens,
	}
	if rec.ExecutionID == "" {
		rec.ExecutionID = newUUIDv4()
	}
	if r.trace != "" {
		rec.TraceID = ptr(r.trace)
	}
	if r.sawBody {
		rec.TTFTMS = ptr(millis(o.firstBody.Sub(o.started)))
	}
	if r.model != "" {
		rec.ResponseModel = ptr(r.model)
	}
	if r.status != 0 {
		rec.Status = ptr(r.status)
	}
	if o.authIndex != "" {
		seat := &Seat{AuthIndex: o.authIndex, Role: roleLastTried}
		if r.status >= 200 && r.status <= 299 {
			seat.Role = roleServed
		}
		if o.bound && o.journal.seats.stillBound(o.authIndex, o.binding, now) {
			seat.File = o.binding.id.file
			seat.Email = o.binding.id.email
			seat.Org = o.binding.id.org
			seat.AccountID = o.binding.id.accountID
		}
		rec.Seat = seat
	}
	return rec, true
}
