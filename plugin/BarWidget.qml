import QtQuick
import QtQuick.Effects
import Quickshell
import qs.Commons
import qs.Ui

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

    // `opened` is the shell's panel-lifecycle contract (bar.summonBarWidget /
    // isBarWidgetOpen look for it), so the popup state lives on this name.
    property bool opened: false
    property int menuIndex: 0

    implicitWidth: button.implicitWidth
    implicitHeight: button.implicitHeight

    function open() { opened = true }
    function close() { opened = false }
    function toggle() { opened = !opened }

    readonly property var menuItems: {
        var items = []
        if (!root.online) {
            items.push({ id: "launch", label: "Launch DroidProxy", action: "launch" })
            return items
        }
        items.push({ id: "settings", label: "Open Settings", action: "openSettings" })
        items.push({ id: "server", label: root.running ? "Stop Server" : "Start Server", action: "serverToggle" })
        items.push({ id: "copyUrl", label: "Copy Server URL", action: "copyUrl", enabled: root.running })
        items.push({ id: "dashboard", label: "Open Dashboard", action: "openDashboard", enabled: root.running })
        if (root.updateAvailable)
            items.push({ id: "update", label: "Update available: v" + (root.dpState.update.latestVersion || "?") + " — Install", action: "installUpdate", highlight: true })
        else
            items.push({ id: "update", label: "Check for Updates…", action: "checkUpdates" })
        items.push({ id: "quit", label: "Quit", action: "quit" })
        return items
    }

    function runMenuAction(action) {
        var svc = root.service
        if (!svc) return
        close()
        switch (action) {
        case "launch":
            svc.launchDaemon()
            break
        case "openSettings":
            if (root.bar && root.bar.shell) root.bar.shell.summon(root.moduleName, "{}")
            break
        case "serverToggle":
            svc.callNotify("server.toggle")
            break
        case "copyUrl":
            svc.callNotify("server.copyUrl")
            break
        case "openDashboard":
            svc.callNotify("open.dashboard")
            break
        case "installUpdate":
            if (root.bar && root.bar.shell) root.bar.shell.summon(root.moduleName, "{}")
            svc.callNotify("update.install")
            break
        case "checkUpdates":
            if (root.bar && root.bar.shell) root.bar.shell.summon(root.moduleName, "{}")
            svc.callNotify("update.check")
            break
        case "quit":
            svc.call("app.quit")
            break
        }
    }

    function statusTooltip() {
        if (!root.online) return "DroidProxy is not running"
        if (!root.dpState) return "DroidProxy"
        return root.running
            ? "DroidProxy: Running (port " + root.dpState.server.proxyPort + ")"
            : "DroidProxy: Stopped"
    }

    BarIconButton {
        id: button
        anchors.fill: parent
        bar: root.bar
        tooltipText: root.statusTooltip()

        iconComponent: Component {
            Item {
                Image {
                    id: iconImage
                    anchors.centerIn: parent
                    width: Style.space(13)
                    height: Style.space(13)
                    source: Qt.resolvedUrl(root.running ? "./assets/icons/icon-active.png" : "./assets/icons/icon-inactive.png")
                    sourceSize.width: 64
                    sourceSize.height: 64
                    fillMode: Image.PreserveAspectFit
                    visible: false
                }
                // The macOS icons are template/mask images; colorization paints
                // them with the bar foreground so they follow the theme.
                MultiEffect {
                    anchors.fill: iconImage
                    source: iconImage
                    colorizationColor: root.running ? root.fg : root.dim
                    colorization: 1.0
                }
            }
        }

        onPressed: function(buttonCode) {
            if (buttonCode === Qt.RightButton && root.online) {
                root.service.callNotify("server.toggle")
            } else {
                root.toggle()
            }
        }
    }

    KeyboardPanel {
        id: panel
        anchorItem: button
        owner: root
        bar: root.bar
        open: root.opened
        focusTarget: keyCatcher
        contentWidth: panel.fittedContentWidth(Style.space(280))
        contentHeight: panel.fittedContentHeight(menuColumn.implicitHeight)

        PanelKeyCatcher {
            id: keyCatcher
            anchors.fill: parent
            onMoveRequested: function(dx, dy) {
                if (dy !== 0) {
                    var count = root.menuItems.length
                    root.menuIndex = (root.menuIndex + dy + count) % count
                }
            }
            onActivateRequested: {
                var item = root.menuItems[root.menuIndex]
                if (item && item.enabled !== false) root.runMenuAction(item.action)
            }
            onCloseRequested: root.close()
        }

        Column {
            id: menuColumn
            width: parent.width
            spacing: 0

            // Header: server status line, mirrors the macOS NSMenu header.
            Text {
                textFormat: Text.PlainText
                width: parent.width
                text: {
                    if (!root.online) return "DroidProxy is not running"
                    var server = root.dpState ? root.dpState.server : null
                    if (server && server.running)
                        return "Server: Running (port " + server.proxyPort + ")"
                    return "Server: Stopped"
                }
                color: root.fg
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                font.bold: true
                leftPadding: Style.space(12)
                rightPadding: Style.space(12)
                topPadding: Style.space(4)
                bottomPadding: Style.space(6)
            }

            PanelSeparator { width: parent.width; foreground: root.fg }

            Repeater {
                model: root.menuItems

                MenuItemRow {
                    width: parent.width
                    label: modelData.label
                    enabledRow: modelData.enabled !== false
                    highlight: modelData.highlight === true
                    selected: root.menuIndex === index
                    fontFamily: root.fontFamily

                    onMouseEntered: root.menuIndex = index
                    onClicked: root.runMenuAction(modelData.action)
                }
            }
        }
    }

    component MenuItemRow: CursorSurface {
        id: row

        property string label: ""
        property bool enabledRow: true
        property bool highlight: false
        property bool selected: false
        property string fontFamily: Style.font.family
        // `baseColor` feeds the inherited CursorSurface.foreground binding, so
        // callers tint rows without fighting the component's own assignment.
        property color baseColor: Color.foreground

        signal clicked()
        signal mouseEntered()

        hasCursor: selected
        foreground: enabledRow ? (highlight ? Color.accent : baseColor) : Qt.darker(baseColor, 1.55)
        implicitHeight: Style.space(32)

        Text {
            textFormat: Text.PlainText
            anchors.verticalCenter: parent.verticalCenter
            anchors.left: parent.left
            anchors.leftMargin: Style.space(12)
            text: row.label
            color: row.enabledRow ? (row.highlight ? Color.accent : row.baseColor) : Qt.darker(row.baseColor, 1.55)
            font.family: row.fontFamily
            font.pixelSize: Style.font.body
            font.bold: row.highlight
        }

        MouseArea {
            anchors.fill: parent
            hoverEnabled: true
            enabled: row.enabledRow
            cursorShape: Qt.PointingHandCursor
            onEntered: row.mouseEntered()
            onClicked: row.clicked()
        }
    }
}
