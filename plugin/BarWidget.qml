import QtQuick
import QtQuick.Effects
import Quickshell
import qs.Commons
import qs.Ui

// DroidProxy bar icon. A single click opens the settings web app in the
// default browser — there is no popup menu anymore. The icon tint and tooltip
// still reflect the server state, and starting the daemon when it is down is
// part of the click.
BarWidget {
    id: root

    moduleName: "nikships.droidproxy"

    readonly property var service: bar && bar.shell ? bar.shell.serviceFor(moduleName) : null
    readonly property var dpState: service ? service.dpState : null
    readonly property bool online: service ? service.online : false
    readonly property bool running: dpState ? dpState.server.running === true : false
    readonly property bool updateAvailable: dpState && dpState.update && dpState.update.state === "available"
    readonly property color fg: bar ? bar.barForeground : Color.foreground
    readonly property color dim: Qt.darker(fg, 1.55)
    readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family

    implicitWidth: button.implicitWidth
    implicitHeight: button.implicitHeight

    function openWebUI() {
        var svc = root.service
        if (!svc) return
        if (!root.online) svc.launchDaemon()
        svc.callNotify("open.webui")
    }

    function statusTooltip() {
        if (!root.online) return "DroidProxy is not running — click to launch and open settings"
        if (!root.dpState) return "DroidProxy — click to open settings"
        var base = root.running
            ? "DroidProxy: Running (port " + root.dpState.server.proxyPort + ")"
            : "DroidProxy: Stopped"
        if (root.updateAvailable)
            base += " — update v" + (root.dpState.update.latestVersion || "?") + " available"
        return base + " — click to open settings"
    }

    BarIconButton {
        id: button
        anchors.fill: parent
        bar: root.bar
        tooltipText: root.statusTooltip()
        // Default canvas is 16px and the slot is 27px. 20px sits between the
        // logo and a full-cell mark.
        opticalSize: Style.space(20)

        iconComponent: Component {
            Item {
                // The macOS assets are black template masks. MultiEffect
                // colorizes by luminance, so a black glyph stays black on
                // the dark bar. These copies are white with the same alpha,
                // which both shows up on its own and tints to the bar color.
                Image {
                    id: iconImage
                    anchors.fill: parent
                    source: Qt.resolvedUrl(root.running ? "./assets/icons/icon-active.png" : "./assets/icons/icon-inactive.png")
                    sourceSize.width: 128
                    sourceSize.height: 128
                    fillMode: Image.PreserveAspectFit
                    layer.enabled: true
                }
                MultiEffect {
                    anchors.fill: iconImage
                    source: iconImage
                    colorization: 1.0
                    colorizationColor: root.running ? root.fg : root.dim
                }
            }
        }

        onPressed: function(buttonCode) {
            root.openWebUI()
        }
    }
}
