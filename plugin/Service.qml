import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons
import "Model.js" as Model

// The shell cannot speak HTTP over a Unix socket, so this service owns
// `droidproxy ctl watch` and one-shot `ctl call` processes. The panel only
// reads the snapshot and asks for actions.
Item {
  id: root

  property var shell: null
  property string pluginId: "nikships.droidproxy"
  property string defaultCommand: "droidproxy"

  property var dpState: null
  property bool online: false
  property string notice: ""

  signal messageArrived(var event)

  readonly property var barEntry: {
    var bar = shell ? shell.barConfig : null
    if (!bar || !Util.isPlainObject(bar.layout)) return null
    var sections = ["left", "centerBegin", "center", "centerEnd", "right", "tray"]
    for (var i = 0; i < sections.length; i++) {
      var entries = bar.layout[sections[i]]
      if (!Array.isArray(entries)) continue
      for (var j = 0; j < entries.length; j++) {
        if (entries[j] && entries[j].id === pluginId) return entries[j]
      }
    }
    return null
  }
  readonly property string command: barEntry && typeof barEntry.command === "string" && barEntry.command !== ""
    ? barEntry.command : defaultCommand

  Process {
    id: watchProc
    stdout: SplitParser {
      onRead: function(line) { root.handleLine(line) }
    }
    onExited: {
      root.online = false
      restartTimer.restart()
    }
  }

  Timer {
    id: restartTimer
    interval: 2000
    onTriggered: root.startWatch()
  }

  Timer {
    id: noticeTimer
    interval: 2400
    onTriggered: root.notice = ""
  }

  onCommandChanged: {
    watchProc.running = false
    restartTimer.restart()
  }

  function startWatch() {
    if (watchProc.running) return
    watchProc.command = [root.command, "ctl", "watch"]
    watchProc.running = true
  }

  function handleLine(line) {
    var event
    try { event = JSON.parse(line) } catch (e) {
      console.warn("droidproxy: bad ctl watch line")
      return
    }
    if (event.type === "state") {
      root.dpState = event.state
      root.online = true
    } else if (event.type === "offline") {
      root.online = false
    } else if (event.type === "message") {
      root.messageArrived(event)
      var body = String(event.body || "")
      if (body !== "") {
        root.showNotice(body)
        Util.execArgv(["notify-send", "-a", "DroidProxy", String(event.title || "DroidProxy"), body])
      }
    }
  }

  function showNotice(text) {
    root.notice = String(text || "")
    if (root.notice !== "") noticeTimer.restart()
  }

  function call(method, params, callback) {
    var body = params === undefined || params === null ? {} : params
    var proc = callProcessComponent.createObject(root, {
      method: method,
      callback: callback || null
    })
    proc.command = [root.command, "ctl", "call", method, JSON.stringify(body)]
    proc.running = true
    return proc
  }

  function launchDaemon() {
    Util.execArgv(["systemctl", "--user", "start", "droidproxy.service"])
  }

  function copyUsage(account, curlText) {
    var accountId = account && (account.id || account.email) || ""
    root.call("usage.copyCurl", { provider: account.provider, accountId: accountId }, function(result) {
      if (result.ok) root.showNotice(curlText)
      else if (result.error) root.showNotice(result.error)
    })
  }

  Component {
    id: callProcessComponent

    Process {
      property string method: ""
      property var callback: null

      stdout: StdioCollector {
        id: callStdout
        waitForEnd: true
      }

      onExited: function(exitCode) {
        var text = (callStdout.text || "").trim()
        var result = null
        if (text !== "") {
          try { result = JSON.parse(text.split("\n").pop()) } catch (e) { result = null }
        }
        if (!result || typeof result.ok !== "boolean") {
          result = { ok: false, error: "DroidProxy did not return a result for " + method + "." }
        }
        if (callback) callback(result)
        destroy()
      }
    }
  }

  // The service IPC target ("droidproxy") opens the bar panel.
  Process { id: panelProc }

  function openPanel() {
    panelProc.running = false
    panelProc.command = ["omarchy-shell", root.pluginId, "open"]
    panelProc.running = true
    return "ok"
  }

  IpcHandler {
    target: "droidproxy"

    function open(): string { return root.openPanel() }
    function ping(): string { return "ok" }
  }

  Component.onCompleted: startWatch()
}
