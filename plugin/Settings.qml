import QtQuick
import QtQuick.Controls
import QtQuick.Effects
import Quickshell
import Quickshell.Wayland
import qs.Commons
import qs.Ui

// DroidProxy settings surface: a centered, scrollable card on a full-screen
// scrim (same pattern as the clipboard overlay). Summoned via IPC
// (`omarchy-shell droidproxy openSettings`) or from the bar widget's quick
// menu. State comes from the plugin's Service singleton.
Item {
    id: root

    property var shell: null
    property var service: null
    property bool opened: false

    readonly property var dpState: service ? service.dpState : null
    readonly property bool online: service ? service.online : false
    readonly property color fg: Color.foreground
    readonly property color dim: Qt.darker(fg, 1.55)
    readonly property color accent: Color.accent
    readonly property color okGreen: "#4caf50"
    readonly property color warnOrange: "#ffa500"
    readonly property color removeRed: "#eb0f0f"
    readonly property string fontFamily: Style.font.family
    readonly property bool oled: s("oledTheme", false)
    readonly property real bgOpacity: s("backgroundOpacity", 1.0)
    readonly property bool beta: s("beta", false)

    // Dialog state: one modal at a time. kind: "" | "result" | "remove" | "junie"
    property string dialogKind: ""
    property var dialogData: ({})

    property bool remoteExpanded: false
    property bool codexFastModeExpanded: true
    property var expandedProviders: ({})

    function s(key, fallback) {
        var snap = dpState && dpState.settings ? dpState.settings : null
        var value = snap ? snap[key] : undefined
        return value === undefined || value === null ? fallback : value
    }

    function open(payloadJson) {
        opened = true
        Qt.callLater(function() { if (root.opened) catcher.forceActiveFocus() })
    }

    // Esc / scrim click close through shell.hide so the host's openPanelIds
    // map stays consistent and `toggleSettings` keeps working.
    function requestClose() {
        if (shell && typeof shell.hide === "function") shell.hide("nikships.droidproxy")
        else opened = false
    }

    function close() { opened = false }

    function showResult(result) {
        var message = result.error && result.error !== "" ? result.error : (result.message || "")
        if (message === "") return
        dialogKind = "result"
        dialogData = {
            title: result.title && result.title !== "" ? result.title : "DroidProxy",
            message: message,
            isError: result.error && result.error !== ""
        }
    }

    onOpenedChanged: {
        if (opened) {
            remoteExpanded = false
            dialogKind = ""
        }
    }

    // Auto-expand provider rows with expired accounts (macOS onAppear/onChange).
    Connections {
        target: root.service
        function onDpStateChanged() {
            if (!root.dpState || !root.dpState.providers) return
            var next = {}
            for (var key in root.expandedProviders) next[key] = root.expandedProviders[key]
            var providers = root.dpState.providers
            for (var i = 0; i < providers.length; i++) {
                var accounts = providers[i].accounts || []
                for (var j = 0; j < accounts.length; j++) {
                    if (accounts[j].expired) { next[providers[i].id] = true; break }
                }
            }
            root.expandedProviders = next
        }
    }

    Connections {
        target: root.service
        function onResultArrived(result) {
            // Action results replace the macOS app's "Authentication Result"
            // alerts: surface them even when the panel is closed by summoning
            // it first, like an alert would have.
            if (!root.opened && root.shell) root.shell.summon("nikships.droidproxy", "{}")
            root.showResult(result)
        }
        function onMessageArrived(event) {
            if (!root.opened && root.shell) root.shell.summon("nikships.droidproxy", "{}")
            root.dialogKind = "result"
            root.dialogData = {
                title: event.title && event.title !== "" ? event.title : "DroidProxy",
                message: event.body || "",
                isError: event.level === "error"
            }
        }
    }

    // ------------------------------------------------------------ window

    PanelWindow {
        id: panel
        visible: root.opened
        anchors { top: true; bottom: true; left: true; right: true }
        color: "transparent"
        WlrLayershell.namespace: "nikships-droidproxy-settings"
        WlrLayershell.layer: WlrLayer.Overlay
        WlrLayershell.keyboardFocus: WlrKeyboardFocus.Exclusive
        exclusionMode: ExclusionMode.Ignore

        Rectangle {
            anchors.fill: parent
            color: Util.alpha(Color.background, 0.5)
        }

        MouseArea {
            anchors.fill: parent
            onClicked: root.requestClose()
        }

        BorderSurface {
            id: card
            anchors.centerIn: parent
            width: Math.min(Style.space(480), panel.width - Style.gapsOut * 4)
            height: Math.min(Style.space(900), panel.height - Style.gapsOut * 4)
            radius: Style.cornerRadius
            color: root.oled ? "#000000" : Util.alpha(Color.popups.background, root.bgOpacity)
            borderSpec: Border.localOrSurfaceSpec("popups", "border", Color.popups.border, Color.popups.border, Math.max(1, Style.space(2)))
            padding: Style.spacing.panelPadding

            // Swallow clicks on the card so they don't reach the scrim's
            // dismiss handler.
            MouseArea { anchors.fill: parent; onClicked: function(mouse) {} }

            // Escape closes the dialog first, then the panel. Everything else
            // (Enter, typing) must reach the fields below, so only Esc is
            // intercepted here.
            Item {
                id: catcher
                anchors.fill: parent
                focus: true
                Keys.priority: Keys.BeforeItem
                Keys.onPressed: function(event) {
                    if (event.key === Qt.Key_Escape) {
                        if (root.dialogKind !== "") root.dialogKind = ""
                        else root.requestClose()
                        event.accepted = true
                    }
                }
            }

            Flickable {
                id: flick
                anchors.fill: parent
                contentWidth: width
                contentHeight: contentColumn.implicitHeight
                clip: true
                boundsBehavior: Flickable.StopAtBounds
                flickableDirection: Flickable.VerticalFlick
                interactive: contentHeight > height
                ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

                Column {
                    id: contentColumn
                    width: flick.width
                    spacing: Style.space(14)

                    // ---------------- offline banner
                    Item {
                        visible: !root.online
                        width: parent.width
                        height: offlineRow.implicitHeight + Style.space(12)

                        Row {
                            id: offlineRow
                            anchors.left: parent.left
                            anchors.verticalCenter: parent.verticalCenter
                            anchors.leftMargin: Style.space(10)
                            spacing: Style.space(10)

                            Text {
                                textFormat: Text.PlainText
                                text: "DroidProxy is not running"
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.body
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            Button {
                                text: "Launch DroidProxy"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                anchors.verticalCenter: parent.verticalCenter
                                onClicked: if (root.service) root.service.launchDaemon()
                            }
                        }
                    }

                    // ---------------- header: logo, beta, opacity, OLED
                    Item {
                        width: parent.width
                        height: Style.space(28)

                        Item {
                            id: logoSlot
                            anchors.left: parent.left
                            anchors.verticalCenter: parent.verticalCenter
                            width: logoImage.width
                            height: Style.space(22)

                            Image {
                                id: logoImage
                                height: Style.space(22)
                                fillMode: Image.PreserveAspectFit
                                source: Qt.resolvedUrl("./assets/logo.svg")
                                sourceSize.height: 64
                                visible: false
                            }

                            MultiEffect {
                                anchors.fill: logoImage
                                source: logoImage
                                colorizationColor: root.fg
                                colorization: 1.0
                            }
                        }

                        Row {
                            anchors.left: logoSlot.right
                            anchors.leftMargin: Style.space(10)
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: Style.space(6)

                            Text {
                                textFormat: Text.PlainText
                                text: "Beta"
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.caption
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            ToggleSwitch {
                                checked: root.beta
                                foreground: root.fg
                                cursorRing: false
                                anchors.verticalCenter: parent.verticalCenter
                                onToggled: root.service.setSetting("beta", !root.beta)

                                PanelToolTip {
                                    visible: parent.containsMouse
                                    text: "Enable beta-gated features"
                                    fontFamily: root.fontFamily
                                }
                            }
                        }

                        Row {
                            anchors.right: oledButton.left
                            anchors.rightMargin: Style.space(10)
                            anchors.verticalCenter: parent.verticalCenter
                            spacing: Style.space(4)

                            Text {
                                textFormat: Text.PlainText
                                text: "◐"
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.caption
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            PanelSlider {
                                width: Style.space(70)
                                minimum: 0.10
                                maximum: 1.0
                                step: 0.05
                                value: root.bgOpacity
                                trackColor: Style.selectedFillFor(root.fg, root.accent)
                                fillColor: root.fg
                                knobColor: root.fg
                                anchors.verticalCenter: parent.verticalCenter
                                onReleased: function(value) { root.service.setSetting("backgroundOpacity", Math.round(value * 100) / 100) }
                            }

                            HoverHandler { id: opacityHover }
                            PanelToolTip {
                                visible: opacityHover.hovered
                                text: "Adjust background opacity (100% = fully opaque)"
                                fontFamily: root.fontFamily
                            }
                        }

                        CursorSurface {
                            id: oledButton
                            width: Style.space(28)
                            height: Style.space(28)
                            foreground: root.fg
                            anchors.right: parent.right
                            anchors.verticalCenter: parent.verticalCenter

                            Text {
                                anchors.centerIn: parent
                                textFormat: Text.PlainText
                                text: root.oled ? "☀" : "☾"
                                color: root.oled ? "#e6d060" : root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.body
                            }

                            MouseArea {
                                anchors.fill: parent
                                cursorShape: Qt.PointingHandCursor
                                onClicked: root.service.setSetting("oledTheme", !root.oled)
                            }

                            HoverHandler { id: oledHover }
                            PanelToolTip {
                                visible: oledHover.hovered
                                text: root.oled ? "Switch to Liquid Glass theme" : "Switch to OLED black theme"
                                fontFamily: root.fontFamily
                            }
                        }
                    }

                    // ---------------- server status
                    Item {
                        width: parent.width
                        height: Style.space(34)

                        Text {
                            textFormat: Text.PlainText
                            anchors.left: parent.left
                            anchors.leftMargin: Style.space(10)
                            anchors.verticalCenter: parent.verticalCenter
                            text: "Server status"
                            color: root.fg
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.body
                            font.bold: true
                        }

                        CursorSurface {
                            id: serverPill
                            width: Style.space(110)
                            height: Style.space(30)
                            foreground: root.fg
                            anchors.right: parent.right
                            anchors.verticalCenter: parent.verticalCenter

                            Row {
                                anchors.centerIn: parent
                                spacing: Style.space(6)

                                Rectangle {
                                    width: Style.space(8)
                                    height: width
                                    radius: width / 2
                                    color: root.online && root.dpState && root.dpState.server.running ? root.okGreen : root.removeRed
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                Text {
                                    textFormat: Text.PlainText
                                    text: root.online && root.dpState && root.dpState.server.running ? "Running" : "Stopped"
                                    color: root.fg
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.caption
                                    anchors.verticalCenter: parent.verticalCenter
                                }
                            }

                            MouseArea {
                                anchors.fill: parent
                                cursorShape: Qt.PointingHandCursor
                                onClicked: root.service.callNotify("server.toggle")
                            }
                        }
                    }

                    // ---------------- OAuth Quota Usage
                    Column {
                        visible: root.online && root.dpState && root.dpState.usage && root.dpState.usage.visible
                        width: parent.width
                        spacing: Style.space(8)

                        Item {
                            width: parent.width
                            height: Math.max(usageHeaderLabel.implicitHeight, usageRefreshButton.implicitHeight)

                            Text {
                                id: usageHeaderLabel
                                textFormat: Text.PlainText
                                text: "OAuth Quota Usage"
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.subtitle
                                font.bold: true
                                anchors.left: parent.left
                                anchors.leftMargin: Style.space(10)
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            Button {
                                id: usageRefreshButton
                                text: root.dpState && root.dpState.usage && root.dpState.usage.refreshing ? "…" : "↻"
                                foreground: root.fg
                                fontFamily: root.fontFamily
                                tooltipText: "Refresh usage quotas"
                                anchors.right: parent.right
                                anchors.verticalCenter: parent.verticalCenter
                                onClicked: root.service.callNotify("usage.refresh")
                            }
                        }

                        Repeater {
                            model: root.dpState && root.dpState.usage ? root.dpState.usage.accounts : []

                            Rectangle {
                                required property var modelData
                                width: parent.width
                                height: usageInner.implicitHeight + Style.space(16)
                                radius: Style.cornerRadius
                                color: Util.alpha(root.fg, 0.05)

                                Column {
                                    id: usageInner
                                    anchors.left: parent.left
                                    anchors.right: parent.right
                                    anchors.verticalCenter: parent.verticalCenter
                                    anchors.margins: Style.space(8)
                                    spacing: Style.space(6)

                                    Row {
                                        width: parent.width
                                        height: Math.max(providerLabel.implicitHeight, emailLabel.implicitHeight)
                                        spacing: Style.space(6)

                                        Text {
                                            id: providerLabel
                                            textFormat: Text.PlainText
                                            text: modelData.providerName
                                            color: root.fg
                                            font.family: root.fontFamily
                                            font.pixelSize: Style.font.caption
                                            font.bold: true
                                            anchors.verticalCenter: parent.verticalCenter
                                        }

                                        Text {
                                            id: emailLabel
                                            textFormat: Text.PlainText
                                            text: modelData.email
                                            color: root.dim
                                            font.family: root.fontFamily
                                            font.pixelSize: Style.font.caption
                                            elide: Text.ElideRight
                                            width: parent.width - providerLabel.implicitWidth - Style.space(12)
                                            anchors.verticalCenter: parent.verticalCenter
                                        }
                                    }

                                    Text {
                                        visible: modelData.error !== ""
                                        text: modelData.error
                                        color: root.warnOrange
                                        font.family: root.fontFamily
                                        font.pixelSize: Style.font.caption
                                        width: parent.width
                                        wrapMode: Text.WordWrap
                                    }

                                    Repeater {
                                        model: modelData.windows

                                        Column {
                                            required property var modelData
                                            width: parent.width
                                            spacing: Style.space(3)

                                            Item {
                                                width: parent.width
                                                height: windowTitle.implicitHeight

                                                Text {
                                                    id: windowTitle
                                                    textFormat: Text.PlainText
                                                    text: modelData.title
                                                    color: root.dim
                                                    font.family: root.fontFamily
                                                    font.pixelSize: Style.font.caption
                                                    anchors.left: parent.left
                                                }

                                                Text {
                                                    textFormat: Text.PlainText
                                                    text: modelData.hasRemaining ? Math.round(modelData.remainingPercent) + "% left" : ""
                                                    color: root.dim
                                                    font.family: root.fontFamily
                                                    font.pixelSize: Style.font.caption
                                                    anchors.right: parent.right
                                                }
                                            }

                                            Rectangle {
                                                visible: modelData.hasRemaining
                                                width: parent.width
                                                height: Style.space(6)
                                                radius: height / 2
                                                color: Util.alpha(root.fg, 0.08)

                                                Rectangle {
                                                    width: parent.width * Math.max(0, Math.min(100, modelData.remainingPercent)) / 100
                                                    height: parent.height
                                                    radius: parent.radius
                                                    color: modelData.remainingPercent < 20 ? root.warnOrange : root.okGreen
                                                }
                                            }

                                            Text {
                                                visible: !modelData.hasRemaining
                                                text: "Usage unavailable"
                                                color: root.dim
                                                font.family: root.fontFamily
                                                font.pixelSize: Style.font.caption
                                            }

                                            Text {
                                                visible: modelData.resetText !== ""
                                                text: modelData.resetText
                                                color: root.dim
                                                font.family: root.fontFamily
                                                font.pixelSize: Style.font.caption
                                            }
                                        }
                                    }
                                }
                            }
                        }

                        Text {
                            visible: root.dpState && root.dpState.usage && root.dpState.usage.accounts.length === 0
                            text: "Connect Codex or Claude OAuth accounts to show quota windows."
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            width: parent.width
                            wrapMode: Text.WordWrap
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- launch / auth files / factory
                    Column {
                        width: parent.width
                        spacing: Style.space(8)

                        Toggle {
                            width: parent.width
                            label: "Launch at login"
                            checked: root.s("launchAtLogin", false)
                            foreground: root.fg
                            onClicked: root.service.setSetting("launchAtLogin", !root.s("launchAtLogin", false))
                        }

                        Item {
                            width: parent.width
                            height: Math.max(authLabel.implicitHeight, authOpenButton.implicitHeight)

                            Text {
                                id: authLabel
                                textFormat: Text.PlainText
                                text: "Auth files"
                                color: root.fg
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.subtitle
                                font.bold: true
                                anchors.left: parent.left
                                anchors.leftMargin: Style.space(10)
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            Button {
                                id: authOpenButton
                                text: "Open Folder"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                anchors.right: parent.right
                                anchors.verticalCenter: parent.verticalCenter
                                onClicked: root.service.callNotify("open.authFolder")
                            }
                        }

                        Column {
                            width: parent.width
                            spacing: Style.space(6)

                            Item {
                                width: parent.width
                                height: Math.max(factoryLabel.implicitHeight, factoryApplyButton.implicitHeight)

                                Text {
                                    id: factoryLabel
                                    textFormat: Text.PlainText
                                    text: "Factory custom models"
                                    color: root.fg
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.subtitle
                                    font.bold: true
                                    anchors.left: parent.left
                                    anchors.leftMargin: Style.space(10)
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                Text {
                                    visible: root.dpState && root.dpState.factory && root.dpState.factory.modelsInstalled
                                    textFormat: Text.PlainText
                                    text: "✓ Applied"
                                    color: root.okGreen
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.caption
                                    anchors.right: factoryApplyButton.left
                                    anchors.rightMargin: Style.space(10)
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                Button {
                                    id: factoryApplyButton
                                    text: root.dpState && root.dpState.factory && root.dpState.factory.modelsInstalled ? "Re-apply" : "Apply"
                                    foreground: root.fg
                                    bordered: true
                                    fontFamily: root.fontFamily
                                    anchors.right: parent.right
                                    anchors.verticalCenter: parent.verticalCenter
                                    onClicked: root.service.callNotify("factory.apply")
                                }
                            }

                            Text {
                                text: "Apply writes DroidProxy model aliases into ~/.factory/settings.json and makes a timestamped backup first. Reasoning effort is selected from Droid CLI when the model exposes multiple levels."
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.caption
                                wrapMode: Text.WordWrap
                                width: parent.width
                            }
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- remote management
                    Column {
                        width: parent.width
                        spacing: Style.space(8)

                        MouseArea {
                            width: parent.width
                            height: remoteHeader.implicitHeight
                            cursorShape: Qt.PointingHandCursor
                            onClicked: root.remoteExpanded = !root.remoteExpanded

                            Row {
                                id: remoteHeader
                                spacing: Style.space(6)

                                Text {
                                    textFormat: Text.PlainText
                                    text: "Remote Management"
                                    color: root.fg
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.subtitle
                                    font.bold: true
                                }

                                Text {
                                    textFormat: Text.PlainText
                                    text: root.remoteExpanded ? "▾" : "▸"
                                    color: root.dim
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.caption
                                    anchors.verticalCenter: parent.verticalCenter
                                }
                            }
                        }

                        // Collapsed summary: "Remote access: On/Off" + warning.
                        Row {
                            visible: !root.remoteExpanded
                            width: parent.width
                            spacing: Style.space(10)

                            Text {
                                textFormat: Text.PlainText
                                text: root.s("allowRemote", false) ? "Remote access: On" : "Remote access: Off"
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.caption
                            }

                            Text {
                                visible: root.s("allowRemote", false) && root.s("secretKey", "") === ""
                                textFormat: Text.PlainText
                                text: "⚠ Secret key missing"
                                color: root.warnOrange
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.caption
                            }
                        }

                        Column {
                            visible: root.remoteExpanded
                            width: parent.width
                            spacing: Style.space(8)

                            Toggle {
                                width: parent.width
                                label: "Allow remote access"
                                checked: root.s("allowRemote", false)
                                foreground: root.fg
                                onClicked: root.service.setSetting("allowRemote", !root.s("allowRemote", false))
                            }

                            Item {
                                width: parent.width
                                height: secretKeyLabel.implicitHeight + Style.space(12)

                                Text {
                                    id: secretKeyLabel
                                    textFormat: Text.PlainText
                                    text: "Secret key"
                                    color: root.fg
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.body
                                    anchors.left: parent.left
                                    anchors.leftMargin: Style.space(10)
                                    anchors.verticalCenter: parent.verticalCenter
                                }

                                TextField {
                                    id: secretKeyField
                                    width: Style.space(200)
                                    password: true
                                    placeholderText: "Enter secret key"
                                    text: root.s("secretKey", "")
                                    foreground: root.fg
                                    anchors.right: parent.right
                                    anchors.verticalCenter: parent.verticalCenter
                                    // Commit on Return or focus loss, like the
                                    // macOS onSubmit — not on every keystroke.
                                    onAccepted: root.service.setSetting("secretKey", text)
                                    onActiveFocusChanged: if (!activeFocus && text !== root.s("secretKey", "")) root.service.setSetting("secretKey", text)
                                }
                            }

                            Column {
                                visible: root.beta
                                width: parent.width
                                spacing: Style.space(6)

                                Item {
                                    width: parent.width
                                    height: bindLabel.implicitHeight + Style.space(12)

                                    Text {
                                        id: bindLabel
                                        textFormat: Text.PlainText
                                        text: "Bind address"
                                        color: root.fg
                                        font.family: root.fontFamily
                                        font.pixelSize: Style.font.body
                                        anchors.left: parent.left
                                        anchors.leftMargin: Style.space(10)
                                        anchors.verticalCenter: parent.verticalCenter
                                    }

                                    TextField {
                                        id: bindAddressField
                                        width: Style.space(200)
                                        placeholderText: "127.0.0.1"
                                        text: root.s("bindAddress", "127.0.0.1")
                                        foreground: root.fg
                                        anchors.right: parent.right
                                        anchors.verticalCenter: parent.verticalCenter
                                        onAccepted: root.service.setSetting("bindAddress", text)
                                        onActiveFocusChanged: if (!activeFocus && text !== root.s("bindAddress", "127.0.0.1")) root.service.setSetting("bindAddress", text)
                                    }
                                }

                                Text {
                                    text: "Default is 127.0.0.1. Set to 0.0.0.0 to allow access from other devices on your network (e.g. Tailscale). Requires server restart."
                                    color: root.dim
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.caption
                                    wrapMode: Text.WordWrap
                                    width: parent.width
                                }
                            }

                            Row {
                                visible: root.s("allowRemote", false) && root.s("secretKey", "") === ""
                                spacing: Style.space(4)

                                Text {
                                    textFormat: Text.PlainText
                                    text: "⚠ Set a secret key to secure remote access"
                                    color: root.warnOrange
                                    font.family: root.fontFamily
                                    font.pixelSize: Style.font.caption
                                }
                            }
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- logging
                    Column {
                        width: parent.width
                        spacing: Style.space(8)

                        PanelSectionHeader {
                            text: "LOGGING"
                            foreground: root.fg
                            fontFamily: root.fontFamily
                        }

                        Toggle {
                            width: parent.width
                            label: "Verbose logging"
                            checked: root.s("verboseLogging", false)
                            foreground: root.fg
                            onClicked: root.service.setSetting("verboseLogging", !root.s("verboseLogging", false))
                        }

                        Item {
                            width: parent.width
                            height: openLogsButton.implicitHeight

                            Button {
                                id: openLogsButton
                                text: "Open Logs"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                anchors.right: parent.right
                                onClicked: root.service.callNotify("open.logsFolder")
                            }
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- account routing
                    Column {
                        width: parent.width
                        spacing: Style.space(6)

                        PanelSectionHeader {
                            text: "ACCOUNT ROUTING"
                            foreground: root.fg
                            fontFamily: root.fontFamily
                        }

                        Toggle {
                            width: parent.width
                            label: "Sequential account failover"
                            checked: root.s("sequentialAccountFailover", false)
                            foreground: root.fg
                            onClicked: root.service.setSetting("sequentialAccountFailover", !root.s("sequentialAccountFailover", false))
                        }

                        Text {
                            text: "With multiple accounts on the same provider, requests stay on one account until its quota runs out, then move to the next automatically without surfacing an error. The exhausted account is skipped until its quota resets. Leave off to spread requests evenly across every account."
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            wrapMode: Text.WordWrap
                            width: parent.width
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- services
                    Column {
                        width: parent.width
                        spacing: Style.space(10)

                        PanelSectionHeader {
                            text: "SERVICES"
                            foreground: root.fg
                            fontFamily: root.fontFamily
                        }

                        Repeater {
                            model: root.dpState ? root.dpState.providers : []

                            ProviderSection {
                                width: parent.width
                                providerData: modelData
                            }
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- updates
                    Column {
                        width: parent.width
                        spacing: Style.space(8)

                        PanelSectionHeader {
                            text: "UPDATES"
                            foreground: root.fg
                            fontFamily: root.fontFamily
                        }

                        Item {
                            width: parent.width
                            height: Math.max(updateStatus.implicitHeight, Math.max(updateCheckButton.height, installButton.height))

                            Text {
                                id: updateStatus
                                textFormat: Text.PlainText
                                text: root.updateStatusText()
                                color: root.dim
                                font.family: root.fontFamily
                                font.pixelSize: Style.font.body
                                width: parent.width - updateCheckButton.width - installButton.width - Style.space(16)
                                wrapMode: Text.WordWrap
                                anchors.verticalCenter: parent.verticalCenter
                            }

                            Button {
                                id: updateCheckButton
                                visible: {
                                    var u = root.dpState ? root.dpState.update : null
                                    return u && (u.state === "idle" || u.state === "upToDate" || u.state === "error")
                                }
                                text: "Check now"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                anchors.right: installButton.visible ? installButton.left : parent.right
                                anchors.rightMargin: Style.space(8)
                                anchors.verticalCenter: parent.verticalCenter
                                onClicked: root.service.callNotify("update.check")
                            }

                            Button {
                                id: installButton
                                visible: {
                                    var u = root.dpState ? root.dpState.update : null
                                    return u && u.state === "available"
                                }
                                text: {
                                    var u = root.dpState ? root.dpState.update : null
                                    return u && u.latestVersion !== "" ? "Install update (v" + u.latestVersion + ")" : "Install update"
                                }
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                anchors.right: parent.right
                                anchors.verticalCenter: parent.verticalCenter
                                onClicked: root.service.callNotify("update.install")
                            }
                        }

                        Rectangle {
                            visible: {
                                var u = root.dpState ? root.dpState.update : null
                                return u && u.state === "downloading"
                            }
                            width: parent.width
                            height: Style.space(6)
                            radius: height / 2
                            color: Util.alpha(root.fg, 0.08)

                            Rectangle {
                                width: parent.width * (root.dpState && root.dpState.update ? root.dpState.update.progress : 0)
                                height: parent.height
                                radius: parent.radius
                                color: root.accent
                            }
                        }

                        Text {
                            visible: {
                                var u = root.dpState ? root.dpState.update : null
                                return u && u.state === "available" && u.notes !== ""
                            }
                            text: root.dpState && root.dpState.update ? root.dpState.update.notes : ""
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            textFormat: Text.PlainText
                            wrapMode: Text.WordWrap
                            width: parent.width
                        }

                        Text {
                            visible: {
                                var u = root.dpState ? root.dpState.update : null
                                return u && u.state === "error" && u.error !== ""
                            }
                            text: root.dpState && root.dpState.update ? root.dpState.update.error : ""
                            color: root.warnOrange
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            wrapMode: Text.WordWrap
                            width: parent.width
                        }

                        Toggle {
                            width: parent.width
                            label: "Automatically check for updates"
                            checked: root.s("autoCheckUpdates", true)
                            foreground: root.fg
                            onClicked: root.service.setSetting("autoCheckUpdates", !root.s("autoCheckUpdates", true))
                        }

                        Toggle {
                            width: parent.width
                            label: "Automatically install updates"
                            checked: root.s("autoInstallUpdates", false)
                            foreground: root.fg
                            onClicked: root.service.setSetting("autoInstallUpdates", !root.s("autoInstallUpdates", false))
                        }
                    }

                    PanelSeparator { foreground: root.fg }

                    // ---------------- footer
                    Column {
                        width: parent.width
                        spacing: Style.space(4)

                        Text {
                            textFormat: Text.RichText
                            width: parent.width
                            horizontalAlignment: Text.AlignHCenter
                            text: {
                                var app = root.dpState ? root.dpState.app : null
                                var version = app ? app.version : ""
                                var cliUrl = app && app.cliProxyApiUrl !== "" ? app.cliProxyApiUrl : "https://github.com/router-for-me/CLIProxyAPI"
                                return "DroidProxy v" + version + " was made possible thanks to "
                                    + "<a href=\"" + cliUrl + "\"><u>CLIProxyAPI</u></a> | License: MIT"
                            }
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            onLinkActivated: function(link) { root.service.callNotify("open.url", { url: link }) }
                        }

                        Text {
                            textFormat: Text.PlainText
                            width: parent.width
                            horizontalAlignment: Text.AlignHCenter
                            text: "© 2026 DroidProxy"
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                        }

                        Item {
                            width: parent.width
                            height: issueButton.implicitHeight + Style.space(8)

                            Button {
                                id: issueButton
                                text: "Report an issue"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                anchors.horizontalCenter: parent.horizontalCenter
                                anchors.verticalCenter: parent.verticalCenter
                                onClicked: {
                                    var app = root.dpState ? root.dpState.app : null
                                    var url = app && app.issuesUrl !== "" ? app.issuesUrl : "https://github.com/nikships/droidproxy-omarchy/issues"
                                    root.service.callNotify("open.url", { url: url })
                                }
                            }
                        }
                    }
                }
            }

            // ---------------- dialogs

            ConfirmDialog {
                anchors.fill: parent
                opened: root.dialogKind === "remove"
                z: 10
                message: root.dialogKind === "remove"
                    ? "Are you sure you want to remove " + root.dialogData.accountName + " from " + root.dialogData.providerName + "?"
                    : ""
                confirmText: "Remove"
                background: root.oled ? "#000000" : Color.popups.background
                foreground: root.fg
                selectedText: root.fg
                fontFamily: root.fontFamily
                onCanceled: root.dialogKind = ""
                onConfirmed: {
                    root.service.callNotify("account.remove", { provider: root.dialogData.providerId, accountId: root.dialogData.accountId })
                    root.dialogKind = ""
                }
            }

            // Result dialog (Authentication Result alerts / async messages).
            Rectangle {
                visible: root.dialogKind === "result"
                anchors.fill: parent
                color: Util.alpha(Color.background, 0.7)
                z: 11

                MouseArea { anchors.fill: parent; onClicked: root.dialogKind = "" }

                BorderSurface {
                    width: Math.min(parent.width - Style.space(32), Style.space(370))
                    height: resultContent.implicitHeight + Style.space(40)
                    anchors.centerIn: parent
                    color: root.oled ? "#000000" : Color.popups.background
                    borderSpec: Border.flat(root.fg, Style.normalBorderWidth)
                    radius: Style.cornerRadius
                    padding: Style.space(18)

                    MouseArea { anchors.fill: parent; onClicked: function(mouse) {} }

                    Column {
                        id: resultContent
                        anchors.left: parent.left
                        anchors.right: parent.right
                        anchors.verticalCenter: parent.verticalCenter
                        spacing: Style.space(12)

                        Text {
                            textFormat: Text.PlainText
                            text: root.dialogData.title || "DroidProxy"
                            color: root.fg
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.title
                            font.bold: true
                        }

                        Text {
                            text: root.dialogData.message || ""
                            color: root.dialogData.isError ? root.warnOrange : root.fg
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.body
                            wrapMode: Text.WordWrap
                            width: parent.width
                        }

                        Button {
                            text: "OK"
                            foreground: root.fg
                            bordered: true
                            fontFamily: root.fontFamily
                            anchors.right: parent.right
                            onClicked: root.dialogKind = ""
                        }
                    }
                }
            }

            // Junie API key dialog.
            Rectangle {
                visible: root.dialogKind === "junie"
                anchors.fill: parent
                color: Util.alpha(Color.background, 0.7)
                z: 11

                MouseArea { anchors.fill: parent; onClicked: root.dialogKind = "" }

                BorderSurface {
                    width: Math.min(parent.width - Style.space(32), Style.space(370))
                    height: junieContent.implicitHeight + Style.space(40)
                    anchors.centerIn: parent
                    color: root.oled ? "#000000" : Color.popups.background
                    borderSpec: Border.flat(root.fg, Style.normalBorderWidth)
                    radius: Style.cornerRadius
                    padding: Style.space(18)

                    MouseArea { anchors.fill: parent; onClicked: function(mouse) {} }

                    Column {
                        id: junieContent
                        anchors.left: parent.left
                        anchors.right: parent.right
                        anchors.verticalCenter: parent.verticalCenter
                        spacing: Style.space(12)

                        Text {
                            textFormat: Text.PlainText
                            text: "Add Junie API Key"
                            color: root.fg
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.title
                            font.bold: true
                        }

                        Text {
                            text: "Please enter your JetBrains Junie API key. It will be saved under ~/.cli-proxy-api/junie.json."
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.body
                            wrapMode: Text.WordWrap
                            width: parent.width
                        }

                        TextField {
                            id: junieField
                            password: true
                            placeholderText: "Enter Junie Key"
                            foreground: root.fg
                            width: parent.width
                            onAccepted: root.saveJunieKey()
                        }

                        Row {
                            anchors.right: parent.right
                            spacing: Style.space(10)

                            Button {
                                text: "Cancel"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                onClicked: root.dialogKind = ""
                            }

                            Button {
                                text: "Save"
                                foreground: root.fg
                                bordered: true
                                fontFamily: root.fontFamily
                                onClicked: root.saveJunieKey()
                            }
                        }
                    }
                }
            }
        }
    }

    function saveJunieKey() {
        if (junieField.text === "") return
        root.service.callNotify("junie.saveKey", { apiKey: junieField.text })
        root.dialogKind = ""
        junieField.text = ""
    }

    function updateStatusText() {
        if (!online || !dpState || !dpState.update) return "DroidProxy is not running"
        var u = dpState.update
        switch (u.state) {
        case "checking": return "Checking for updates…"
        case "upToDate": return "Up to date (v" + u.currentVersion + ")"
        case "available": return "Update available: v" + u.latestVersion
        case "downloading": return "Downloading update… " + Math.round(u.progress * 100) + "%"
        case "installing": return "Installing update…"
        case "error": return "Update check failed: " + u.error
        default: return "Current version: v" + u.currentVersion
        }
    }

    // One provider block in the Services section: header row (toggle, icon,
    // name, add button / spinner), optional device-code and fast-mode extras,
    // and the collapsible account list.
    component ProviderSection: Column {
        id: providerSection

        property var providerData: null

        readonly property color providerColor: providerData && providerData.color !== "" ? providerData.color : root.accent
        // Only providers with more than one account offer the enable/disable
        // per-row toggle (last-enabled protection lives on the daemon).
        readonly property bool showAccountToggles: providerData && providerData.accounts && providerData.accounts.length > 1
        readonly property bool expanded: root.isProviderExpanded(providerData ? providerData.id : "")
        readonly property bool hasExpired: {
            if (!providerData || !providerData.accounts) return false
            for (var i = 0; i < providerData.accounts.length; i++)
                if (providerData.accounts[i].expired) return true
            return false
        }

        spacing: Style.space(6)

        // -------- header row: toggle, icon, name, add/spinner
        Item {
            width: parent.width
            height: Math.max(Style.space(24), providerAddButton.implicitHeight)

            ToggleSwitch {
                checked: providerData.enabled
                foreground: providerSection.providerColor
                cursorRing: false
                anchors.left: parent.left
                anchors.verticalCenter: parent.verticalCenter
                onToggled: root.service.callNotify("provider.setEnabled", { provider: providerData.id, enabled: !providerData.enabled })
            }

            Item {
                width: Style.space(20)
                height: Style.space(20)
                anchors.left: parent.left
                anchors.leftMargin: Style.space(34)
                anchors.verticalCenter: parent.verticalCenter

                Image {
                    id: providerIcon
                    anchors.centerIn: parent
                    width: Style.space(18)
                    height: Style.space(18)
                    source: Qt.resolvedUrl("./assets/icons/" + providerData.icon)
                    sourceSize.height: 48
                    fillMode: Image.PreserveAspectFit
                    visible: false
                }

                MultiEffect {
                    anchors.fill: providerIcon
                    source: providerIcon
                    colorizationColor: root.fg
                    colorization: 1.0
                    opacity: providerData.enabled ? 1.0 : 0.4
                }
            }

            Text {
                textFormat: Text.PlainText
                text: providerData.name
                color: providerData.enabled ? root.fg : root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                font.bold: true
                anchors.left: parent.left
                anchors.leftMargin: Style.space(60)
                anchors.verticalCenter: parent.verticalCenter
            }

            Text {
                visible: providerData.authenticating
                textFormat: Text.PlainText
                text: "↻"
                color: root.fg
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
                anchors.right: parent.right
                anchors.rightMargin: Style.space(4)
                anchors.verticalCenter: parent.verticalCenter

                RotationAnimation on rotation {
                    running: providerData.authenticating
                    from: 0
                    to: 360
                    duration: 900
                    loops: Animation.Infinite
                }
            }

            Button {
                id: providerAddButton
                visible: !providerData.authenticating && providerData.enabled
                text: "Add Account"
                foreground: providerSection.providerColor
                fontFamily: root.fontFamily
                anchors.right: parent.right
                anchors.verticalCenter: parent.verticalCenter
                onClicked: {
                    if (providerData.kind === "junie") {
                        root.dialogKind = "junie"
                    } else {
                        root.service.callNotify("provider.connect", { provider: providerData.id })
                    }
                }
            }

            HoverHandler { id: providerRowHover }
            PanelToolTip {
                visible: providerData.help !== "" && providerRowHover.hovered
                text: providerData.help
                fontFamily: root.fontFamily
            }
        }

        // -------- grok device-code row
        Item {
            visible: providerData.kind === "grok" && providerData.enabled && providerData.authenticating
            width: parent.width
            height: grokContent.implicitHeight

            Column {
                id: grokContent
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.leftMargin: Style.space(28)
                spacing: Style.space(4)

                Text {
                    textFormat: Text.PlainText
                    text: "Enter this code at the verification link:"
                    color: root.dim
                    font.family: root.fontFamily
                    font.pixelSize: Style.font.caption
                }

                Row {
                    spacing: Style.space(8)

                    Text {
                        textFormat: Text.PlainText
                        text: root.dpState && root.dpState.grok ? root.dpState.grok.userCode : ""
                        color: root.fg
                        font.family: root.fontFamily
                        font.pixelSize: Style.font.body
                        font.bold: true
                        anchors.verticalCenter: parent.verticalCenter
                    }

                    Button {
                        text: "Copy"
                        foreground: root.fg
                        bordered: true
                        fontFamily: root.fontFamily
                        onClicked: root.service.callNotify("clipboard.copy", { text: root.dpState.grok.userCode })
                    }

                    Button {
                        text: "Open Grok"
                        foreground: root.fg
                        bordered: true
                        fontFamily: root.fontFamily
                        onClicked: root.service.callNotify("open.url", { url: root.dpState.grok.verificationUrl })
                    }

                    Button {
                        text: "Cancel sign-in"
                        foreground: root.fg
                        bordered: true
                        fontFamily: root.fontFamily
                        onClicked: root.service.callNotify("provider.cancelAuth", { provider: providerData.id })
                    }
                }
            }
        }

        // -------- meta extras
        Column {
            visible: providerData.kind === "meta" && providerData.enabled
            width: parent.width
            spacing: Style.space(4)

            Item {
                visible: root.dpState && root.dpState.meta && root.dpState.meta.authenticating
                width: parent.width
                height: metaAuthContent.implicitHeight

                Column {
                    id: metaAuthContent
                    anchors.left: parent.left
                    anchors.right: parent.right
                    anchors.leftMargin: Style.space(28)
                    spacing: Style.space(4)

                    Text {
                        textFormat: Text.PlainText
                        text: "Complete Meta sign-in with this device code:"
                        color: root.dim
                        font.family: root.fontFamily
                        font.pixelSize: Style.font.caption
                    }

                    Row {
                        spacing: Style.space(8)

                        Text {
                            textFormat: Text.PlainText
                            text: root.dpState && root.dpState.meta ? root.dpState.meta.deviceCode : ""
                            color: root.fg
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.body
                            font.bold: true
                            anchors.verticalCenter: parent.verticalCenter
                        }

                        Button {
                            text: "Copy"
                            foreground: root.fg
                            bordered: true
                            fontFamily: root.fontFamily
                            onClicked: root.service.callNotify("clipboard.copy", { text: root.dpState.meta.deviceCode })
                        }

                        Button {
                            text: "Open Meta"
                            foreground: root.fg
                            bordered: true
                            fontFamily: root.fontFamily
                            onClicked: root.service.callNotify("open.url", { url: root.dpState.meta.verificationUrl })
                        }

                        Button {
                            text: "Cancel sign-in"
                            foreground: root.fg
                            bordered: true
                            fontFamily: root.fontFamily
                            onClicked: root.service.callNotify("provider.cancelAuth", { provider: providerData.id })
                        }
                    }
                }
            }

            Text {
                visible: root.dpState && root.dpState.meta && root.dpState.meta.lastError !== ""
                text: root.dpState && root.dpState.meta ? root.dpState.meta.lastError : ""
                color: root.warnOrange
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                wrapMode: Text.WordWrap
                width: parent.width
            }

            Item {
                visible: providerData.enabled
                width: parent.width
                height: museRow.implicitHeight

                Row {
                    id: museRow
                    anchors.right: parent.right
                    anchors.rightMargin: Style.space(10)
                    spacing: Style.space(8)

                    Text {
                        textFormat: Text.PlainText
                        text: "Muse Spark"
                        color: root.dim
                        font.family: root.fontFamily
                        font.pixelSize: Style.font.caption
                        anchors.verticalCenter: parent.verticalCenter
                    }

                    Text {
                        textFormat: Text.PlainText
                        text: "Contributor mode"
                        color: root.dim
                        font.family: root.fontFamily
                        font.pixelSize: Style.font.caption
                        anchors.verticalCenter: parent.verticalCenter
                    }

                    ToggleSwitch {
                        checked: root.s("metaContributorMode", false)
                        foreground: root.fg
                        cursorRing: false
                        anchors.verticalCenter: parent.verticalCenter
                        onToggled: root.service.setSetting("metaContributorMode", !root.s("metaContributorMode", false))

                        PanelToolTip {
                            visible: parent.containsMouse
                            text: "Applies Muse Spark 1.3 Contributor instead of Muse Spark 1.3 when Factory custom models are applied. Only one is ever active."
                            fontFamily: root.fontFamily
                        }
                    }
                }
            }
        }

        // -------- codex fast mode
        Item {
            visible: providerData.id === "codex" && providerData.enabled
            width: parent.width
            height: fastModeContent.implicitHeight

            Column {
                id: fastModeContent
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.leftMargin: Style.space(28)
                spacing: Style.space(4)

                MouseArea {
                    width: parent.width
                    height: fastModeHeader.implicitHeight
                    cursorShape: Qt.PointingHandCursor
                    onClicked: root.codexFastModeExpanded = !root.codexFastModeExpanded

                    Row {
                        id: fastModeHeader
                        spacing: Style.space(4)

                        Text {
                            textFormat: Text.PlainText
                            text: "Fast Mode"
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                        }

                        Text {
                            textFormat: Text.PlainText
                            text: root.codexFastModeExpanded ? "▾" : "▸"
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            anchors.verticalCenter: parent.verticalCenter
                        }
                    }
                }

                Repeater {
                    model: root.codexFastModeExpanded ? [
                        { key: "gpt6AstraFastMode", title: "GPT 6 Astra", help: "Injects service_tier=priority for GPT 6 Astra Responses API requests (Codex fast mode)" },
                        { key: "gpt6SolFastMode", title: "GPT 6 Sol", help: "Injects service_tier=priority for GPT 6 Sol Responses API requests (Codex fast mode)" },
                        { key: "gpt6LunaFastMode", title: "GPT 6 Luna", help: "Injects service_tier=priority for GPT 6 Luna Responses API requests (Codex fast mode)" }
                    ] : []

                    Item {
                        required property var modelData
                        width: parent.width
                        height: Math.max(fastModeTitle.implicitHeight, fastModeSwitch.height)

                        Text {
                            id: fastModeTitle
                            textFormat: Text.PlainText
                            text: modelData.title
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            anchors.left: parent.left
                            anchors.verticalCenter: parent.verticalCenter
                        }

                        ToggleSwitch {
                            id: fastModeSwitch
                            checked: root.s(modelData.key, false)
                            foreground: root.fg
                            cursorRing: false
                            anchors.right: parent.right
                            anchors.rightMargin: Style.space(10)
                            anchors.verticalCenter: parent.verticalCenter
                            onToggled: root.service.setSetting(modelData.key, !root.s(modelData.key, false))

                            PanelToolTip {
                                visible: fastModeSwitch.containsMouse
                                text: modelData.help
                                fontFamily: root.fontFamily
                            }
                        }
                    }
                }
            }
        }

        // -------- accounts
        Item {
            visible: providerData.enabled
            width: parent.width
            height: accountsContent.implicitHeight

            Column {
                id: accountsContent
                anchors.left: parent.left
                anchors.right: parent.right
                anchors.leftMargin: Style.space(28)
                spacing: Style.space(4)

                Text {
                    visible: !providerData.accounts || providerData.accounts.length === 0
                    textFormat: Text.PlainText
                    text: "No connected accounts"
                    color: root.dim
                    font.family: root.fontFamily
                    font.pixelSize: Style.font.caption
                }

                MouseArea {
                    visible: providerData.accounts && providerData.accounts.length > 0
                    width: parent.width
                    height: accountSummary.implicitHeight + Style.space(8)
                    cursorShape: Qt.PointingHandCursor
                    onClicked: root.toggleProviderExpanded(providerData.id)

                    Row {
                        id: accountSummary
                        spacing: Style.space(4)

                        Text {
                            textFormat: Text.PlainText
                            text: providerData.accounts.length + " connected account" + (providerData.accounts.length === 1 ? "" : "s")
                            color: providerSection.providerColor
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                        }

                        Text {
                            visible: root.enabledCount(providerData) > 1
                            textFormat: Text.PlainText
                            text: root.s("sequentialAccountFailover", false) ? "• Sequential auto-failover" : "• Round-robin w/ auto-failover"
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            anchors.verticalCenter: parent.verticalCenter
                        }

                        Text {
                            textFormat: Text.PlainText
                            text: providerSection.expanded ? "▾" : "▸"
                            color: root.dim
                            font.family: root.fontFamily
                            font.pixelSize: Style.font.caption
                            anchors.verticalCenter: parent.verticalCenter
                        }
                    }
                }

                Column {
                    visible: providerSection.expanded
                    width: parent.width
                    spacing: Style.space(4)

                    Repeater {
                        model: providerData.accounts

                        AccountRow {
                            required property var modelData
                            width: parent.width
                            accountData: modelData
                            providerColor: providerSection.providerColor
                            showToggle: providerSection.showAccountToggles
                            providerId: providerData.id
                            providerName: providerData.name
                        }
                    }
                }
            }
        }
    }

    component AccountRow: Item {
        id: accountRow

        property var accountData: null
        property color providerColor: root.accent
        property bool showToggle: false
        property string providerId: ""
        property string providerName: ""

        readonly property bool canDisable: accountData.disabled || root.enabledCountFor(providerId) > 1

        width: parent.width
        height: Style.space(22)

        Rectangle {
            width: Style.space(6)
            height: Style.space(6)
            radius: width / 2
            anchors.left: parent.left
            anchors.leftMargin: Style.space(8)
            anchors.verticalCenter: parent.verticalCenter
            color: accountData.disabled ? root.dim
                : (accountData.expired ? Util.alpha(accountRow.providerColor, 0.6) : accountRow.providerColor)
        }

        Text {
            id: accountName
            textFormat: Text.PlainText
            text: {
                var name = accountData.displayName
                if (accountData.expired && !accountData.disabled) name += "  (expired)"
                if (accountData.disabled) name += "  (disabled)"
                return name
            }
            color: accountData.disabled ? Util.alpha(root.fg, 0.5) : (accountData.expired ? Util.alpha(accountRow.providerColor, 0.8) : root.dim)
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
            font.strikeout: accountData.disabled
            anchors.left: parent.left
            anchors.leftMargin: Style.space(24)
            anchors.verticalCenter: parent.verticalCenter
            width: parent.width - anchors.leftMargin - (accountToggle.visible ? accountToggle.width : 0) - (accountRemove.visible ? accountRemove.width : 0) - Style.space(40)
            elide: Text.ElideRight
        }

        Text {
            id: accountToggle
            visible: accountRow.showToggle
            textFormat: Text.PlainText
            text: accountData.disabled ? "Enable" : "Disable"
            color: {
                if (accountData.disabled) return accountRow.providerColor
                return accountRow.canDisable ? Util.alpha(accountRow.providerColor, 0.6) : Util.alpha(root.fg, 0.35)
            }
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
            anchors.right: accountRemove.visible ? accountRemove.left : parent.right
            anchors.rightMargin: Style.space(10)
            anchors.verticalCenter: parent.verticalCenter

            MouseArea {
                anchors.fill: parent
                cursorShape: accountRow.canDisable ? Qt.PointingHandCursor : Qt.ArrowCursor
                enabled: accountRow.canDisable
                onClicked: root.service.callNotify("account.toggleDisabled", { provider: accountRow.providerId, accountId: accountData.id })
            }

            HoverHandler { id: toggleHover }
            PanelToolTip {
                visible: !accountData.disabled && !accountRow.canDisable && toggleHover.hovered
                text: "At least one account must remain enabled"
                fontFamily: root.fontFamily
            }
        }

        Item {
            id: accountRemove
            width: removeGlyph.implicitWidth + Style.space(2) + removeLabel.implicitWidth
            height: removeLabel.implicitHeight
            anchors.right: parent.right
            anchors.verticalCenter: parent.verticalCenter

            Text {
                id: removeGlyph
                textFormat: Text.PlainText
                text: "⊖"
                color: root.removeRed
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                anchors.left: parent.left
                anchors.verticalCenter: parent.verticalCenter
            }

            Text {
                id: removeLabel
                textFormat: Text.PlainText
                text: "Remove"
                color: root.removeRed
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
                anchors.left: removeGlyph.right
                anchors.leftMargin: Style.space(2)
                anchors.verticalCenter: parent.verticalCenter
            }

            MouseArea {
                anchors.fill: parent
                cursorShape: Qt.PointingHandCursor
                onClicked: {
                    root.dialogKind = "remove"
                    root.dialogData = {
                        providerId: accountRow.providerId,
                        providerName: accountRow.providerName,
                        accountId: accountData.id,
                        accountName: accountData.displayName
                    }
                }
            }
        }
    }

    function isProviderExpanded(providerId) {
        return expandedProviders[providerId] === true
    }

    function toggleProviderExpanded(providerId) {
        var next = {}
        for (var key in expandedProviders) next[key] = expandedProviders[key]
        next[providerId] = !isProviderExpanded(providerId)
        expandedProviders = next
    }

    function enabledCount(provider) {
        return enabledCountFor(provider.id)
    }

    function enabledCountFor(providerId) {
        if (!dpState || !dpState.providers) return 0
        for (var i = 0; i < dpState.providers.length; i++) {
            if (dpState.providers[i].id !== providerId) continue
            var count = 0
            var accounts = dpState.providers[i].accounts || []
            for (var j = 0; j < accounts.length; j++)
                if (!accounts[j].disabled) count++
            return count
        }
        return 0
    }
}
