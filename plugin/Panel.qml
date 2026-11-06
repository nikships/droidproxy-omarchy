import QtQuick
import QtQuick.Controls
import QtQuick.Effects
import QtQuick.Layouts
import Quickshell
import Quickshell.Io
import qs.Commons
import qs.Commons as Commons
import qs.Ui
import "Model.js" as Model

Panel {
  id: root
  moduleName: "nikships.droidproxy"
  ipcTarget: "nikships.droidproxy"

  Commons.Translations { id: strings; directory: Qt.resolvedUrl("i18n") }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  readonly property var service: bar && bar.shell ? bar.shell.serviceFor(moduleName) : null
  readonly property var dpState: service ? service.dpState : null
  readonly property bool online: service ? service.online === true : false
  readonly property bool running: dpState && dpState.server ? dpState.server.running === true : false
  readonly property bool starting: dpState && dpState.server ? dpState.server.starting === true : false
  readonly property string proxyPort: dpState && dpState.server && dpState.server.proxyPort ? String(dpState.server.proxyPort) : ""
  readonly property bool updateAvailable: dpState && dpState.update && dpState.update.state === "available"
  readonly property string latestVersion: dpState && dpState.update ? String(dpState.update.latestVersion || "") : ""
  readonly property var providers: Model.providerList(dpState)
  readonly property var usage: Model.usageAccounts(dpState)
  readonly property var pendingSignIn: Model.signIn(dpState)
  readonly property string serverError: Model.serverError(dpState)
  readonly property bool modelsInstalled: dpState && dpState.factory ? dpState.factory.modelsInstalled === true : false
  readonly property bool betaOn: flag("beta", Model.setting(dpState, "beta", false) === true)
  readonly property bool remoteOn: flag("allowRemote", Model.setting(dpState, "allowRemote", false) === true)
  readonly property bool secretSet: String(Model.setting(dpState, "secretKey", "") || "") !== ""
  readonly property string bindAddress: String(Model.setting(dpState, "bindAddress", "127.0.0.1") || "127.0.0.1")

  property bool advancedOpen: false
  property bool cursorActive: false
  property string cursorId: "server"
  property bool editing: false
  property bool justApplied: false
  property var optimistic: ({})
  property string confirmProvider: ""
  property string confirmAccount: ""
  property string confirmName: ""
  property string copiedUsageId: ""

  // The released daemon clears "authenticating" while the browser login
  // helper is still holding the callback port, and cancelAuth does not stop
  // that helper. The panel watches for it and stops it on Cancel.
  property var loginPending: ({})
  property var loginLive: ({})
  property var loginAnchor: ({})
  property var loginDeadline: ({})
  property int loginEpoch: 0

  readonly property bool confirming: confirmAccount !== ""
  readonly property color foreground: bar ? bar.foreground : Commons.Color.foreground
  readonly property color urgent: bar ? bar.urgent : Commons.Color.urgent
  readonly property color dim: Qt.darker(foreground, 1.45)
  readonly property color accent: Commons.Color.accent
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family
  readonly property color hoverFill: bar ? Style.hoverFillFor(foreground, accent) : "transparent"
  readonly property string notice: service && service.notice ? service.notice : ""

  function flag(key, live) {
    if (optimistic[key] !== undefined) return optimistic[key]
    return live
  }

  function expect(key, value) {
    var next = {}
    for (var name in optimistic) next[name] = optimistic[name]
    next[key] = value
    optimistic = next
  }

  function drop(key) {
    var next = {}
    for (var name in optimistic) if (name !== key) next[name] = optimistic[name]
    optimistic = next
  }

  onDpStateChanged: optimistic = ({})

  function entries() {
    return Model.cursorEntries(dpState, { advanced: advancedOpen })
  }

  function hot(id) {
    return cursorActive && cursorId === id
  }

  function aim(id) {
    cursorId = id
    cursorActive = true
  }

  function moveCursor(dx, dy) {
    if (dx !== 0 && cursorId.indexOf("primer:") === 0) {
      var primer = Model.primer(dpState, cursorId.slice("primer:".length))
      if (primer && service) callSetting(primer.minutesKey, Model.shiftMinutes(flag(primer.minutesKey, primer.minutes), dx * 30))
    }
    if (dy === 0) return
    var list = entries()
    if (list.length === 0) return
    if (!cursorActive) {
      cursorActive = true
      return
    }
    var index = Model.entryIndex(list, cursorId)
    if (index < 0) index = 0
    else index = Math.max(0, Math.min(list.length - 1, index + dy))
    cursorId = list[index].id
  }

  function callSetting(key, value) {
    if (!service) return
    expect(key, value)
    service.call("settings.set", { key: key, value: value }, function(result) {
      if (!result.ok) {
        root.drop(key)
        if (result.error) service.showNotice(result.error)
      }
    })
  }

  function toggleSetting(key) {
    var live = Model.setting(dpState, key, false) === true
    callSetting(key, !flag(key, live))
  }

  function toggleServer() {
    if (!service) return
    if (!online) {
      service.launchDaemon()
      return
    }
    if (starting) return
    service.call("server.toggle", {})
  }

  function toggleProvider(provider) {
    if (!service || !provider) return
    var next = !flag("provider:" + provider.id, provider.enabled)
    expect("provider:" + provider.id, next)
    service.call("provider.setEnabled", { provider: provider.id, enabled: next }, function(result) {
      if (!result.ok) {
        root.drop("provider:" + provider.id)
        if (result.error) service.showNotice(result.error)
      }
    })
  }

  function accountOn(providerId, account) {
    return flag("account:" + providerId + ":" + account.id, !account.disabled)
  }

  function toggleAccount(providerId, account) {
    if (!service || !account) return
    // The control call flips the account. The switch shows "on" when it is not disabled.
    expect("account:" + providerId + ":" + account.id, account.disabled)
    service.call("account.toggleDisabled", { provider: providerId, accountId: account.id }, function(result) {
      if (!result.ok) {
        root.drop("account:" + providerId + ":" + account.id)
        if (result.error) service.showNotice(result.error)
      } else if (result.message) service.showNotice(result.message)
    })
  }

  function accountById(providerId, accountId) {
    var provider = Model.providerById(dpState, providerId)
    if (!provider) return null
    for (var i = 0; i < provider.accounts.length; i++)
      if (provider.accounts[i].id === accountId) return provider.accounts[i]
    return null
  }

  function askRemove(providerId, account) {
    if (!account) return
    confirmProvider = providerId
    confirmAccount = account.id
    confirmName = account.displayName
  }

  function cancelRemove() {
    confirmProvider = ""
    confirmAccount = ""
    confirmName = ""
  }

  function confirmRemove() {
    if (!service || confirmAccount === "") return
    var providerId = confirmProvider
    var accountId = confirmAccount
    cancelRemove()
    service.call("account.remove", { provider: providerId, accountId: accountId }, function(result) {
      if (!result.ok && result.error) service.showNotice(result.error)
      else if (result.message) service.showNotice(result.message)
    })
  }

  function applyFactory() {
    if (!service) return
    service.call("factory.apply", {}, function(result) {
      if (result.ok) {
        root.justApplied = true
        appliedTimer.restart()
        service.showNotice(strings.t("notice.applied"))
      } else if (result.error) service.showNotice(result.error)
    })
  }

  function copyUsage(index) {
    if (!service) return
    var account = usage[index]
    if (!account) return
    copiedUsageId = String(index)
    copiedTimer.restart()
    service.copyUsage(
      account,
      strings.t("quota.copied").arg(account.providerName || account.provider),
      strings.t("quota.copiedSummary").arg(account.providerName || account.provider)
    )
  }

  function saveJunie(field) {
    if (!service || !field) return
    var value = String(field.text || "").trim()
    if (value === "") return
    service.call("junie.saveKey", { apiKey: value }, function(result) {
      if (result.ok) field.text = ""
      if (result.ok && result.message) service.showNotice(result.message)
      else if (!result.ok && result.error) service.showNotice(result.error)
    })
  }

  function report(result, okText) {
    if (!service || !result) return
    if (result.ok) {
      if (okText) service.showNotice(okText)
    } else if (result.error) service.showNotice(result.error)
  }

  function providerBusy(id) {
    if (loginEpoch >= 0 && loginActive(id)) return true
    if (pendingSignIn && pendingSignIn.provider === id) return true
    var provider = Model.providerById(dpState, id)
    return !!(provider && provider.authenticating)
  }

  function connectProvider(id) {
    if (!service || !id) return
    service.call("provider.connect", { provider: id }, function(result) {
      if (result.ok) root.noteLoginStarted(id)
      root.report(result, result.message || "")
    })
  }

  function cancelProvider(id) {
    if (!service || !root.loginProvider(id)) return
    var pending = root.copyMap(loginPending)
    var anchor = root.copyMap(loginAnchor)
    var deadline = root.copyMap(loginDeadline)
    delete pending[id]
    delete anchor[id]
    delete deadline[id]
    loginPending = pending
    loginAnchor = anchor
    loginDeadline = deadline
    loginEpoch = loginEpoch + 1
    service.call("provider.cancelAuth", { provider: id })
    service.showNotice(strings.t("notice.cancelled"))
    if (id === "grok" || id === "meta") {
      root.scanLogins()
      return
    }
    var proc = loginProcessComponent.createObject(root, {})
    proc.command = ["/usr/bin/python3", "-B", root.loginScript(), "cancel", id]
    proc.running = true
  }

  function loginActive(id) {
    return loginPending[id] === true || loginLive[id] === true
  }

  function noteLoginStarted(id) {
    if (!root.loginProvider(id)) return
    var pending = root.copyMap(loginPending)
    var anchor = root.copyMap(loginAnchor)
    var deadline = root.copyMap(loginDeadline)
    pending[id] = true
    anchor[id] = "wait"
    deadline[id] = Date.now() + 8000
    loginPending = pending
    loginAnchor = anchor
    loginDeadline = deadline
    loginEpoch = loginEpoch + 1
    root.scanLogins()
  }

  function loginScript() {
    var url = Qt.resolvedUrl("login-procs.py").toString()
    if (url.indexOf("file://") === 0) url = url.slice("file://".length)
    return decodeURIComponent(url)
  }

  function loginProvider(id) {
    return id === "claude" || id === "codex" || id === "antigravity" || id === "kimi" || id === "grok" || id === "meta"
  }

  function copyMap(source) {
    var next = {}
    for (var key in source) next[key] = source[key]
    return next
  }

  function sameFlags(left, right) {
    var key
    for (key in left) if (left[key] && !right[key]) return false
    for (key in right) if (right[key] && !left[key]) return false
    return true
  }

  function scanLogins() {
    if (scanProc.running) {
      scanAgain.restart()
      return
    }
    scanProc.command = ["/usr/bin/python3", "-B", root.loginScript(), "scan"]
    scanProc.running = true
  }

  function applyScan(text) {
    var live = {}
    var parts = String(text || "").trim().split(/\s+/)
    for (var i = 0; i < parts.length; i++) {
      if (parts[i] === "") continue
      var pair = parts[i].split("=")
      if (pair.length === 2 && pair[0] !== "" && pair[1] !== "") live[pair[0]] = true
    }
    var now = Date.now()
    var ids = {}
    var key
    for (key in loginPending) ids[key] = true
    for (key in loginAnchor) ids[key] = true
    for (key in live) ids[key] = true
    var pending = {}
    var anchor = {}
    var deadline = {}
    for (key in ids) {
      if (live[key]) {
        pending[key] = true
        anchor[key] = "seen"
        deadline[key] = loginDeadline[key] || 0
      } else if (loginAnchor[key] === "wait" && now < (loginDeadline[key] || 0)) {
        pending[key] = true
        anchor[key] = "wait"
        deadline[key] = loginDeadline[key]
      }
    }
    var changed = !root.sameFlags(loginLive, live) || !root.sameFlags(loginPending, pending)
    loginAnchor = anchor
    loginDeadline = deadline
    if (!root.sameFlags(loginLive, live)) loginLive = live
    if (!root.sameFlags(loginPending, pending)) loginPending = pending
    if (changed) loginEpoch = loginEpoch + 1
  }

  function activate() {
    if (!service) return
    var list = entries()
    var index = Model.entryIndex(list, cursorId)
    if (index < 0) return
    var entry = list[index]
    if (entry.kind === "server") toggleServer()
    else if (entry.kind === "copy-url") service.call("server.copyUrl", {}, function(result) { root.report(result, strings.t("notice.copiedUrl")) })
    else if (entry.kind === "factory") applyFactory()
    else if (entry.kind === "refresh") service.call("usage.refresh", {})
    else if (entry.kind === "usage") copyUsage(entry.index)
    else if (entry.kind === "provider") toggleProvider(Model.providerById(dpState, entry.provider))
    else if (entry.kind === "account") toggleAccount(entry.provider, accountById(entry.provider, entry.accountId))
    else if (entry.kind === "connect") {
      if (providerBusy(entry.provider)) cancelProvider(entry.provider)
      else connectProvider(entry.provider)
    }
    else if (entry.kind === "signin-copy" && pendingSignIn) service.call("clipboard.copy", { text: pendingSignIn.code }, function(result) { root.report(result, strings.t("notice.copiedCode")) })
    else if (entry.kind === "signin-open") service.call("open.url", { url: entry.url })
    else if (entry.kind === "signin-cancel") cancelProvider(entry.provider)
    else if (entry.kind === "primer") {
      var primer = Model.primer(dpState, entry.provider)
      if (primer) callSetting(primer.enabledKey, !flag(primer.enabledKey, primer.enabled))
    }
    else if (entry.kind === "fast" || entry.kind === "setting") toggleSetting(entry.key)
    else if (entry.kind === "open") service.call(entry.method, {})
    else if (entry.kind === "update-check") service.call("update.check", {})
    else if (entry.kind === "update-install") service.call("update.install", {}, function(result) { root.report(result, result.message || "") })
    else if (entry.kind === "advanced") advancedOpen = !advancedOpen
  }

  function removeFocusedAccount() {
    var list = entries()
    var index = Model.entryIndex(list, cursorId)
    if (index < 0 || list[index].kind !== "account") return
    askRemove(list[index].provider, accountById(list[index].provider, list[index].accountId))
  }

  function inkFor(providerId, remaining) {
    if (remaining >= 0 && remaining < 15) return urgent
    var provider = Model.providerById(dpState, providerId)
    if (provider && provider.color !== "") return provider.color
    return foreground
  }

  function heroMeta() {
    if (!service) return strings.t("hero.offline")
    if (starting) return strings.t("hero.starting")
    if (!online) return strings.t("hero.offline")
    if (running) return strings.t("hero.running")
    return strings.t("hero.stopped")
  }

  function updateLabel() {
    var update = dpState && dpState.update
    if (!update || !update.state || update.state === "idle") return strings.t("update.idle")
    if (update.state === "available") return strings.t("update.available").arg(latestVersion)
    if (update.state === "checking") return strings.t("update.checking")
    if (update.state === "downloading") return strings.t("update.downloading")
    if (update.state === "installing") return strings.t("update.installing")
    if (update.state === "upToDate") return strings.t("update.uptodate")
    if (update.state === "error") return update.error ? String(update.error) : strings.t("update.error")
    return strings.t("update.idle")
  }

  Timer {
    id: appliedTimer
    interval: 1200
    onTriggered: root.justApplied = false
  }

  Timer {
    id: copiedTimer
    interval: 1000
    onTriggered: root.copiedUsageId = ""
  }

  Timer {
    interval: 1500
    repeat: true
    running: true
    onTriggered: root.scanLogins()
  }

  Timer {
    id: scanAgain
    interval: 400
    onTriggered: root.scanLogins()
  }

  Process {
    id: scanProc
    stdout: StdioCollector {
      id: scanOut
      waitForEnd: true
    }
    onExited: root.applyScan(scanOut.text)
  }

  Component {
    id: loginProcessComponent
    Process {
      onExited: {
        root.scanLogins()
        destroy()
      }
    }
  }

  Component.onCompleted: root.scanLogins()

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    opticalSize: Style.space(20)
    tooltipText: !root.online
      ? strings.t("tooltip.offline")
      : (!root.running
        ? strings.t("tooltip.stopped")
        : (root.updateAvailable
          ? strings.t("tooltip.runningUpdate").arg(root.proxyPort).arg(root.latestVersion)
          : strings.t("tooltip.running").arg(root.proxyPort)))

    iconComponent: Component {
      Item {
        Image {
          id: iconImage
          anchors.fill: parent
          source: Qt.resolvedUrl(root.running ? "assets/icons/icon-active.png" : "assets/icons/icon-inactive.png")
          sourceSize.width: 128
          sourceSize.height: 128
          fillMode: Image.PreserveAspectFit
        }
        MultiEffect {
          anchors.fill: iconImage
          source: iconImage
          colorization: 1
          colorizationColor: root.running ? root.foreground : root.dim
        }
        Rectangle {
          visible: root.updateAvailable
          width: Style.space(6)
          height: width
          radius: width / 2
          color: root.urgent
          anchors.right: parent.right
          anchors.top: parent.top
        }
      }
    }

    onPressed: function(buttonCode) {
      if (buttonCode === Qt.RightButton) root.toggleServer()
      else root.toggle()
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: panel.loadedContent
    contentWidth: panel.fittedContentWidth(Style.space(420))
    contentHeight: panel.fittedContentHeight(panel.loadedContentHeight, Style.space(640))

    content: PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      implicitHeight: column.implicitHeight
      blocked: root.editing
      onMoveRequested: function(dx, dy) {
        if (root.confirming) {
          if (dx !== 0) confirmDialog.selectedIndex = confirmDialog.selectedIndex === 0 ? 1 : 0
          return
        }
        root.moveCursor(dx, dy)
      }
      onActivateRequested: root.confirming ? root.confirmRemove() : root.activate()
      onCloseRequested: root.confirming ? root.cancelRemove() : root.close()
      onDeleteRequested: if (!root.confirming) root.removeFocusedAccount()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(text) {
        if (text === "r" || text === "R") {
          if (root.service) root.service.call("usage.refresh", {})
        }
      }

      Flickable {
        id: panelFlick
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: panelFlick.width
          spacing: Style.space(14)

          PanelHero {
            id: hero
            width: parent.width
            title: strings.t("hero.title")
            meta: root.heroMeta()
            detail: root.running && root.proxyPort !== "" ? strings.t("hero.port").arg(root.proxyPort) : ""
            foreground: root.foreground
            fontFamily: root.fontFamily
            iconOpacity: root.running ? 1 : 0.55
            iconComponent: Component {
              Image {
                source: Qt.resolvedUrl("assets/icons/icon-active.png")
                sourceSize.width: 96
                sourceSize.height: 96
                fillMode: Image.PreserveAspectFit
                width: Style.font.display
                height: width
              }
            }
            trailingControl: Component {
              ToggleSwitch {
                checked: root.running
                busy: root.starting
                hasCursor: root.hot("server")
                foreground: hero.foreground
                onHovered: function(on) { if (on) root.aim("server") }
                onToggled: root.toggleServer()
              }
            }
          }

          Text {
            visible: root.serverError !== ""
            width: parent.width
            textFormat: Text.PlainText
            wrapMode: Text.WordWrap
            text: root.serverError
            color: root.urgent
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
          }

          Button {
            width: parent.width
            text: strings.t("copy.url")
            bordered: true
            hasCursor: root.hot("copy-url")
            foreground: root.foreground
            fontFamily: root.fontFamily
            visible: root.dpState && root.dpState.server && String(root.dpState.server.url || "") !== ""
            onHovered: function(on) { if (on) root.aim("copy-url") }
            onClicked: root.service && root.service.call("server.copyUrl", {}, function(result) { root.report(result, strings.t("notice.copiedUrl")) })
          }

          Column {
            width: parent.width
            spacing: Style.space(8)

            Text {
              width: parent.width
              textFormat: Text.PlainText
              text: strings.t("factory.title")
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.subtitle
              font.bold: true
            }
            Text {
              width: parent.width
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: root.modelsInstalled ? strings.t("factory.applied") : strings.t("factory.missing")
              color: root.modelsInstalled ? root.dim : root.accent
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
            Text {
              width: parent.width
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: strings.t("factory.blurb")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
            Button {
              width: parent.width
              text: root.justApplied ? strings.t("factory.just") : (root.modelsInstalled ? strings.t("factory.reapply") : strings.t("factory.apply"))
              active: !root.modelsInstalled && !root.justApplied
              bordered: true
              hasCursor: root.hot("factory")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("factory") }
              onClicked: root.applyFactory()
            }
          }

          Column {
            visible: root.pendingSignIn !== null
            width: parent.width
            spacing: Style.space(8)

            PanelSectionHeader {
              text: strings.t("auth.code")
              foreground: root.foreground
              fontFamily: root.fontFamily
            }
            Text {
              width: parent.width
              textFormat: Text.PlainText
              text: root.pendingSignIn ? root.pendingSignIn.code : ""
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.title
              font.bold: true
            }
            Text {
              visible: root.pendingSignIn && root.pendingSignIn.error !== ""
              width: parent.width
              wrapMode: Text.WordWrap
              textFormat: Text.PlainText
              text: root.pendingSignIn ? root.pendingSignIn.error : ""
              color: root.urgent
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
            RowLayout {
              width: parent.width
              spacing: Style.space(8)
              Button {
                text: strings.t("auth.copy")
                bordered: true
                hasCursor: root.hot("signin-copy")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("signin-copy") }
                onClicked: root.service && root.pendingSignIn && root.service.call("clipboard.copy", { text: root.pendingSignIn.code }, function(result) { root.report(result, strings.t("notice.copiedCode")) })
              }
              Button {
                visible: root.pendingSignIn && root.pendingSignIn.url !== ""
                text: strings.t("auth.open")
                bordered: true
                hasCursor: root.hot("signin-open")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("signin-open") }
                onClicked: root.service && root.pendingSignIn && root.service.call("open.url", { url: root.pendingSignIn.url })
              }
              Button {
                text: strings.t("auth.cancel")
                bordered: true
                hasCursor: root.hot("signin-cancel")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("signin-cancel") }
                onClicked: root.pendingSignIn && root.cancelProvider(root.pendingSignIn.provider)
              }
            }
          }

          Column {
            width: parent.width
            spacing: Style.space(10)

            RowLayout {
              width: parent.width
              PanelSectionHeader {
                text: strings.t("section.quota")
                foreground: root.foreground
                fontFamily: root.fontFamily
                Layout.fillWidth: true
              }
              Button {
                visible: root.usage.length > 0
                text: strings.t("quota.refresh")
                hasCursor: root.hot("refresh")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("refresh") }
                onClicked: root.service && root.service.call("usage.refresh", {})
              }
            }

            Text {
              visible: root.usage.length === 0
              width: parent.width
              wrapMode: Text.WordWrap
              textFormat: Text.PlainText
              text: strings.t("quota.empty")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
            }

            Repeater {
              model: root.usage
              delegate: Column {
                id: usageCard
                required property int index
                required property var modelData
                width: column.width
                spacing: Style.space(6)

                Rectangle {
                  anchors.fill: parent
                  radius: Style.cornerRadius
                  color: root.hot("usage:" + usageCard.index) ? root.hoverFill : "transparent"
                  z: -1
                }

                Text {
                  width: parent.width
                  textFormat: Text.PlainText
                  elide: Text.ElideRight
                  text: modelData.email !== "" ? modelData.email : modelData.providerName
                  color: root.foreground
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.body
                  font.bold: true
                }
                Text {
                  visible: modelData.email !== "" && modelData.providerName !== ""
                  width: parent.width
                  textFormat: Text.PlainText
                  text: modelData.providerName
                  color: root.dim
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                }
                Text {
                  visible: modelData.error !== ""
                  width: parent.width
                  wrapMode: Text.WordWrap
                  textFormat: Text.PlainText
                  text: modelData.error
                  color: root.urgent
                  font.family: root.fontFamily
                  font.pixelSize: Style.font.caption
                }
                Row {
                  spacing: Style.space(6)
                  Repeater {
                    model: usageCard.modelData.windows
                    delegate: Gauge {
                      required property int index
                      required property var modelData
                      remaining: modelData.remaining
                      caption: modelData.title
                      tip: modelData.resetText !== "" ? modelData.resetText : (modelData.remaining < 0 ? strings.t("quota.unavailable") : "")
                      ink: root.inkFor(usageCard.modelData.provider, modelData.remaining)
                      fontFamily: root.fontFamily
                      hasCursor: root.hot("usage:" + usageCard.index)
                      confirmed: root.copiedUsageId === String(usageCard.index)
                      onClicked: root.copyUsage(usageCard.index)
                    }
                  }
                }
              }
            }
          }

          PanelSeparator { foreground: root.foreground }

          Column {
            width: parent.width
            spacing: Style.space(12)

            PanelSectionHeader {
              text: strings.t("section.services")
              foreground: root.foreground
              fontFamily: root.fontFamily
            }

            Repeater {
              model: root.providers
              delegate: providerCard
            }
          }

          PanelSeparator { foreground: root.foreground }

          Column {
            width: parent.width
            spacing: Style.space(8)

            Button {
              width: parent.width
              text: strings.t("section.advanced")
              bordered: true
              hasCursor: root.hot("advanced")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("advanced") }
              onClicked: root.advancedOpen = !root.advancedOpen
            }
            Text {
              visible: !root.advancedOpen
              width: parent.width
              textFormat: Text.PlainText
              wrapMode: Text.WordWrap
              text: strings.t("advanced.summary")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }

            Toggle {
              visible: root.advancedOpen
              width: parent.width
              label: strings.t("setting.launch")
              checked: root.flag("launchAtLogin", Model.setting(root.dpState, "launchAtLogin", false) === true)
              hasCursor: root.hot("launch")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("launch") }
              onClicked: root.toggleSetting("launchAtLogin")
            }
            Toggle {
              visible: root.advancedOpen
              width: parent.width
              label: strings.t("setting.failover")
              description: strings.t("setting.failoverHelp")
              checked: root.flag("sequentialAccountFailover", Model.setting(root.dpState, "sequentialAccountFailover", false) === true)
              hasCursor: root.hot("failover")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("failover") }
              onClicked: root.toggleSetting("sequentialAccountFailover")
            }
            Toggle {
              visible: root.advancedOpen
              width: parent.width
              label: strings.t("setting.verbose")
              description: strings.t("setting.verboseHelp")
              checked: root.flag("verboseLogging", Model.setting(root.dpState, "verboseLogging", false) === true)
              hasCursor: root.hot("verbose")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("verbose") }
              onClicked: root.toggleSetting("verboseLogging")
            }
            Toggle {
              visible: root.advancedOpen
              width: parent.width
              label: strings.t("setting.remote")
              checked: root.remoteOn
              hasCursor: root.hot("remote")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("remote") }
              onClicked: root.toggleSetting("allowRemote")
            }
            Text {
              visible: root.advancedOpen && root.remoteOn && !root.secretSet
              width: parent.width
              wrapMode: Text.WordWrap
              textFormat: Text.PlainText
              text: strings.t("setting.secretMissing")
              color: root.urgent
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
            Text {
              visible: root.advancedOpen && root.secretSet
              width: parent.width
              wrapMode: Text.WordWrap
              textFormat: Text.PlainText
              text: strings.t("setting.secretSaved")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
            TextField {
              id: secretField
              visible: root.advancedOpen
              width: parent.width
              password: true
              placeholderText: strings.t("setting.secret")
              foreground: root.foreground
              font.family: root.fontFamily
              onActiveFocusChanged: root.editing = activeFocus
              Keys.onEscapePressed: function(event) {
                focus = false
                root.editing = false
                event.accepted = true
              }
              onAccepted: {
                var value = text
                text = ""
                root.callSetting("secretKey", value)
              }
            }
            Toggle {
              visible: root.advancedOpen
              width: parent.width
              label: strings.t("setting.beta")
              checked: root.betaOn
              hasCursor: root.hot("beta")
              foreground: root.foreground
              fontFamily: root.fontFamily
              onHovered: function(on) { if (on) root.aim("beta") }
              onClicked: root.toggleSetting("beta")
            }
            Text {
              visible: root.advancedOpen && root.betaOn
              width: parent.width
              wrapMode: Text.WordWrap
              textFormat: Text.PlainText
              text: strings.t("setting.bindHelp")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
            }
            TextField {
              id: bindField
              visible: root.advancedOpen && root.betaOn
              width: parent.width
              placeholderText: root.bindAddress
              foreground: root.foreground
              font.family: root.fontFamily
              onActiveFocusChanged: root.editing = activeFocus
              Keys.onEscapePressed: function(event) {
                focus = false
                root.editing = false
                event.accepted = true
              }
              onAccepted: root.callSetting("bindAddress", text)
            }

            RowLayout {
              visible: root.advancedOpen
              width: parent.width
              spacing: Style.space(8)
              Button {
                text: strings.t("open.auth")
                bordered: true
                hasCursor: root.hot("auth-folder")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("auth-folder") }
                onClicked: root.service && root.service.call("open.authFolder", {})
              }
              Button {
                text: strings.t("open.logs")
                bordered: true
                hasCursor: root.hot("logs-folder")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("logs-folder") }
                onClicked: root.service && root.service.call("open.logsFolder", {})
              }
            }

            Text {
              visible: root.advancedOpen
              width: parent.width
              textFormat: Text.PlainText
              text: root.updateLabel()
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
            }
            RowLayout {
              visible: root.advancedOpen
              width: parent.width
              spacing: Style.space(8)
              Button {
                text: strings.t("update.check")
                bordered: true
                hasCursor: root.hot("update-check")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("update-check") }
                onClicked: root.service && root.service.call("update.check", {})
              }
              Button {
                visible: root.updateAvailable
                text: strings.t("update.install")
                bordered: true
                hasCursor: root.hot("update-install")
                foreground: root.foreground
                fontFamily: root.fontFamily
                onHovered: function(on) { if (on) root.aim("update-install") }
                onClicked: root.service && root.service.call("update.install", {}, function(result) { root.report(result, result.message || "") })
              }
            }
          }
        }
      }

      BorderSurface {
        z: 2
        visible: root.notice !== ""
        anchors.horizontalCenter: parent.horizontalCenter
        anchors.top: parent.top
        width: Math.min(toastText.implicitWidth + Style.space(24), parent.width - Style.space(16))
        implicitHeight: toastText.implicitHeight + Style.space(14)
        radius: Style.cornerRadius
        color: Qt.rgba(root.foreground.r, root.foreground.g, root.foreground.b, 0.14)
        borderSpec: Border.controlSpec("normal", root.foreground, root.accent)

        Text {
          id: toastText
          anchors.centerIn: parent
          width: parent.width - Style.space(20)
          horizontalAlignment: Text.AlignHCenter
          wrapMode: Text.WordWrap
          textFormat: Text.PlainText
          text: root.notice
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
        }
      }

      ConfirmDialog {
        id: confirmDialog
        anchors.fill: parent
        z: 3
        opened: root.confirming
        message: strings.t("account.removeConfirm").arg(root.confirmName)
        confirmText: strings.t("account.remove")
        foreground: root.foreground
        fontFamily: root.fontFamily
        onOpenedChanged: if (opened) selectedIndex = 1
        onCanceled: root.cancelRemove()
        onConfirmed: root.confirmRemove()
      }
    }
  }

  Component {
    id: providerCard

    Column {
      id: providerColumn
      required property int index
      required property var modelData
      width: parent ? parent.width : 0
      spacing: Style.space(8)

      Item {
        width: parent.width
        implicitHeight: providerRow.implicitHeight + Style.space(8)

        Rectangle {
          anchors.fill: parent
          radius: Style.cornerRadius
          color: root.hot("provider:" + providerColumn.modelData.id) ? root.hoverFill : "transparent"
        }

        RowLayout {
          id: providerRow
          anchors.left: parent.left
          anchors.right: parent.right
          anchors.verticalCenter: parent.verticalCenter
          anchors.margins: Style.space(4)
          spacing: Style.space(10)

          Item {
            Layout.preferredWidth: Style.space(22)
            Layout.preferredHeight: Style.space(22)
            Image {
              id: providerIcon
              anchors.fill: parent
              visible: providerColumn.modelData.icon !== ""
              source: providerColumn.modelData.icon !== "" ? Qt.resolvedUrl("assets/icons/" + providerColumn.modelData.icon) : ""
              fillMode: Image.PreserveAspectFit
              sourceSize.width: 64
              sourceSize.height: 64
            }
            MultiEffect {
              anchors.fill: providerIcon
              source: providerIcon
              visible: Model.tintIcon(providerColumn.modelData.icon)
              colorization: 1
              colorizationColor: providerColumn.modelData.color !== "" ? providerColumn.modelData.color : root.foreground
            }
            Text {
              anchors.centerIn: parent
              visible: providerColumn.modelData.icon === ""
              textFormat: Text.PlainText
              text: providerColumn.modelData.name.charAt(0)
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              font.bold: true
            }
          }

          ColumnLayout {
            Layout.fillWidth: true
            spacing: 0
            Text {
              Layout.fillWidth: true
              textFormat: Text.PlainText
              elide: Text.ElideRight
              text: providerColumn.modelData.name
              color: root.foreground
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              font.bold: true
            }
            Text {
              Layout.fillWidth: true
              textFormat: Text.PlainText
              elide: Text.ElideRight
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.caption
              text: {
                var provider = providerColumn.modelData
                if (provider.authenticating || root.loginActive(provider.id)) return strings.t("provider.signingIn")
                var count = provider.accounts.length
                if (count === 0) return strings.t("account.none")
                if (count === 1) return provider.accounts[0].displayName
                return strings.tn("accounts.count", count).arg(Commons.I18n.formatNumber(count))
              }
            }
          }

          ToggleSwitch {
            interactive: false
            checked: root.flag("provider:" + providerColumn.modelData.id, providerColumn.modelData.enabled)
            hasCursor: root.hot("provider:" + providerColumn.modelData.id)
            foreground: root.foreground
          }
        }

        MouseArea {
          anchors.fill: parent
          hoverEnabled: true
          onEntered: root.aim("provider:" + providerColumn.modelData.id)
          onClicked: root.toggleProvider(providerColumn.modelData)
        }
      }

      Text {
        visible: providerColumn.modelData.enabled && providerColumn.modelData.help !== ""
        width: parent.width
        leftPadding: Style.space(32)
        wrapMode: Text.WordWrap
        textFormat: Text.PlainText
        text: providerColumn.modelData.help
        color: root.dim
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
      }

      Repeater {
        model: providerColumn.modelData.enabled ? providerColumn.modelData.accounts : []
        delegate: RowLayout {
          id: accountRow
          required property int index
          required property var modelData
          width: providerColumn.width
          spacing: Style.space(8)

          Item {
            Layout.fillWidth: true
            Layout.leftMargin: Style.space(32)
            Layout.preferredHeight: accountLabel.implicitHeight + Style.space(8)

            Rectangle {
              anchors.fill: parent
              radius: Style.cornerRadius
              color: root.hot("account:" + providerColumn.modelData.id + ":" + accountRow.modelData.id) ? root.hoverFill : "transparent"
            }

            Column {
              id: accountLabel
              anchors.left: parent.left
              anchors.right: accountSwitch.left
              anchors.rightMargin: Style.space(8)
              anchors.verticalCenter: parent.verticalCenter
              spacing: 0
              Text {
                width: parent.width
                textFormat: Text.PlainText
                elide: Text.ElideRight
                text: accountRow.modelData.displayName
                color: root.foreground
                font.family: root.fontFamily
                font.pixelSize: Style.font.body
              }
              Text {
                visible: accountRow.modelData.expired || accountRow.modelData.disabled
                width: parent.width
                textFormat: Text.PlainText
                text: accountRow.modelData.expired ? strings.t("account.expired") : strings.t("account.disabled")
                color: accountRow.modelData.expired ? root.urgent : root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
              }
            }

            ToggleSwitch {
              id: accountSwitch
              anchors.right: parent.right
              anchors.verticalCenter: parent.verticalCenter
              interactive: false
              checked: root.accountOn(providerColumn.modelData.id, accountRow.modelData)
              hasCursor: root.hot("account:" + providerColumn.modelData.id + ":" + accountRow.modelData.id)
              foreground: root.foreground
            }

            MouseArea {
              anchors.fill: parent
              hoverEnabled: true
              onEntered: root.aim("account:" + providerColumn.modelData.id + ":" + accountRow.modelData.id)
              onClicked: root.toggleAccount(providerColumn.modelData.id, accountRow.modelData)
            }
          }

          Button {
            text: strings.t("account.remove")
            foreground: root.foreground
            fontFamily: root.fontFamily
            onClicked: root.askRemove(providerColumn.modelData.id, accountRow.modelData)
          }
        }
      }

      Button {
        visible: providerColumn.modelData.enabled && providerColumn.modelData.id !== "junie"
        text: root.providerBusy(providerColumn.modelData.id) ? strings.t("auth.cancelSignIn") : strings.t("account.add")
        bordered: true
        hasCursor: root.hot("connect:" + providerColumn.modelData.id)
        foreground: root.foreground
        fontFamily: root.fontFamily
        onHovered: function(on) { if (on) root.aim("connect:" + providerColumn.modelData.id) }
        onClicked: {
          var id = providerColumn.modelData.id
          if (root.providerBusy(id)) root.cancelProvider(id)
          else root.connectProvider(id)
        }
      }

      RowLayout {
        visible: providerColumn.modelData.enabled && providerColumn.modelData.id === "junie"
        width: parent.width
        spacing: Style.space(8)
        TextField {
          id: junieField
          Layout.fillWidth: true
          password: true
          placeholderText: strings.t("junie.placeholder")
          foreground: root.foreground
          font.family: root.fontFamily
          onActiveFocusChanged: root.editing = activeFocus
          Keys.onEscapePressed: function(event) {
            focus = false
            root.editing = false
            event.accepted = true
          }
          onAccepted: root.saveJunie(junieField)
        }
        Button {
          text: strings.t("junie.save")
          bordered: true
          hasCursor: root.hot("junie")
          foreground: root.foreground
          fontFamily: root.fontFamily
          onHovered: function(on) { if (on) root.aim("junie") }
          onClicked: root.saveJunie(junieField)
        }
      }
      Text {
        visible: providerColumn.modelData.enabled && providerColumn.modelData.id === "junie"
        width: parent.width
        textFormat: Text.PlainText
        text: strings.t("junie.help")
        color: root.dim
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
      }

      RowLayout {
        visible: providerColumn.modelData.enabled && Model.primer(root.dpState, providerColumn.modelData.id) !== null
        width: parent.width
        spacing: Style.space(8)
        Toggle {
          Layout.fillWidth: true
          label: {
            var primer = Model.primer(root.dpState, providerColumn.modelData.id)
            var minutes = primer ? root.flag(primer.minutesKey, primer.minutes) : 0
            return strings.t("setting.primer").arg(Model.clockLabel(minutes))
          }
          description: strings.t("setting.primerHelp")
          checked: {
            var primer = Model.primer(root.dpState, providerColumn.modelData.id)
            return primer ? root.flag(primer.enabledKey, primer.enabled) : false
          }
          hasCursor: root.hot("primer:" + providerColumn.modelData.id)
          foreground: root.foreground
          fontFamily: root.fontFamily
          onHovered: function(on) { if (on) root.aim("primer:" + providerColumn.modelData.id) }
          onClicked: {
            var primer = Model.primer(root.dpState, providerColumn.modelData.id)
            if (primer) root.callSetting(primer.enabledKey, !root.flag(primer.enabledKey, primer.enabled))
          }
        }
      }

      Column {
        visible: providerColumn.modelData.enabled && providerColumn.modelData.id === "codex" && Model.fastModes(root.dpState).length > 0
        width: parent.width
        spacing: Style.space(8)
        Text {
          width: parent.width
          textFormat: Text.PlainText
          text: strings.t("setting.fast")
          color: root.foreground
          font.family: root.fontFamily
          font.pixelSize: Style.font.body
          font.bold: true
        }
        Text {
          width: parent.width
          wrapMode: Text.WordWrap
          textFormat: Text.PlainText
          text: strings.t("setting.fastHelp")
          color: root.dim
          font.family: root.fontFamily
          font.pixelSize: Style.font.caption
        }
        Repeater {
          model: Model.fastModes(root.dpState)
          delegate: Toggle {
            required property var modelData
            width: providerColumn.width
            label: modelData.label
            checked: root.flag(modelData.key, Model.setting(root.dpState, modelData.key, false) === true)
            hasCursor: root.hot("fast:" + modelData.key)
            foreground: root.foreground
            fontFamily: root.fontFamily
            onHovered: function(on) { if (on) root.aim("fast:" + modelData.key) }
            onClicked: root.toggleSetting(modelData.key)
          }
        }
      }

      Toggle {
        visible: providerColumn.modelData.enabled && providerColumn.modelData.id === "meta" && typeof Model.setting(root.dpState, "metaContributorMode", null) === "boolean"
        width: parent.width
        label: strings.t("setting.contributor")
        checked: root.flag("metaContributorMode", Model.setting(root.dpState, "metaContributorMode", false) === true)
        hasCursor: root.hot("contributor")
        foreground: root.foreground
        fontFamily: root.fontFamily
        onHovered: function(on) { if (on) root.aim("contributor") }
        onClicked: root.toggleSetting("metaContributorMode")
      }
    }
  }
}
