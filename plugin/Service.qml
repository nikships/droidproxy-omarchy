import QtQuick
import Quickshell
import Quickshell.Io
import qs.Commons

// Headless singleton: owns the `ctl watch` process, the parsed dpState snapshot,
// one-shot `ctl call` processes, and the "droidproxy" IPC target. The bar
// widget and settings panel read dpState from here (injected as `service`)
// instead of each running their own watcher.
Item {
    id: root

    property var shell: null
    property string pluginId: "nikships.droidproxy"
    property string defaultCommand: "droidproxy"

    // Full state snapshot from the daemon, null while offline/unknown. Named
    // dpState because Item already owns a `state` property.
    property var dpState: null
    property bool online: false
    property bool updating: false

    signal resultArrived(var result)
    signal messageArrived(var event)

    // The widget's `command` setting lives inline on the bar layout entry in
    // shell.json; the service (a separate mount of the same plugin) reads it
    // from the live bar config so both stay in sync without extra plumbing.
    // shell.barConfig is the `bar:` subtree itself, so layout is a direct key.
    readonly property var barEntry: {
        var bar = shell ? shell.barConfig : null
        if (!bar || !Util.isPlainObject(bar.layout)) return null
        var sections = ["left", "center", "right"]
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

    function setting(key, fallback) {
        var s = dpState && dpState.settings
        var value = s ? s[key] : undefined
        return value === undefined || value === null ? fallback : value
    }

    // ------------------------------------------------------------ watch process

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
        interval: 1000
        onTriggered: root.startWatch()
    }

    onCommandChanged: {
        // Restart so a new mock/CLI override takes effect immediately.
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
            console.warn("droidproxy: bad ctl watch line:", line)
            return
        }
        if (event.type === "state") {
            root.dpState = event.state
            root.online = true
        } else if (event.type === "offline") {
            root.online = false
        } else if (event.type === "message") {
            root.messageArrived(event)
        }
    }

    Component.onCompleted: startWatch()

    // ------------------------------------------------------------ ctl calls

    // Runs `<command> ctl call <method> '<json>'` and parses the stdout JSON
    // action result. callback(result) receives the parsed object (ok/message/
    // error/title) or a synthetic {ok:false,error:...} when the CLI itself
    // failed to run or returned garbage.
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

    // call() plus the standard result-dialog surfacing: any non-empty
    // message/error from the daemon ends up in the settings panel's dialog.
    function callNotify(method, params) {
        root.call(method, params, function(result) {
            if (result && ((result.message && result.message !== "") || (result.error && result.error !== "")))
                root.resultArrived(result)
        })
    }

    function setSetting(key, value) {
        root.call("settings.set", { key: key, value: value })
    }

    function launchDaemon() {
        Util.execDetached("systemctl --user start droidproxy.service")
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
                    result = { ok: false, error: "DroidProxy CLI did not return a valid result for " + method + "." }
                }
                if (root.resultArrived && callback) callback(result)
                destroy()
            }
        }
    }

    // ------------------------------------------------------------ IPC

    IpcHandler {
        target: "droidproxy"

        function openSettings(): string {
            if (!root.shell) return "no-shell"
            return root.shell.summon(root.pluginId, "{}") ? "ok" : "unknown"
        }

        function closeSettings(): string {
            if (!root.shell) return "no-shell"
            root.shell.hide(root.pluginId)
            return "ok"
        }

        function toggleSettings(): string {
            if (!root.shell) return "no-shell"
            root.shell.toggle(root.pluginId, "{}")
            return "ok"
        }

        function toggleMenu(): string {
            if (!root.shell || !root.shell.bar) return "no-bar"
            if (root.shell.bar.isBarWidgetOpen(root.pluginId))
                root.shell.bar.hideBarWidget(root.pluginId)
            else
                root.shell.bar.summonBarWidget(root.pluginId)
            return "ok"
        }

        function openMenu(): string {
            if (!root.shell || !root.shell.bar) return "no-bar"
            return root.shell.bar.summonBarWidget(root.pluginId) ? "ok" : "unknown"
        }

        function ping(): string {
            return "ok"
        }
    }
}
