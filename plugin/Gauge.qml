import QtQuick
import qs.Commons as Commons
import qs.Ui

// Remaining quota as a ring. The number and caption are plain text; the
// stroke only moves when a new reading arrives.
Item {
  id: root

  property real remaining: -1
  property string caption: ""
  property string tip: ""
  property color ink: Commons.Color.foreground
  property bool hasCursor: false
  property bool confirmed: false
  property string fontFamily: Style.font.family

  signal clicked()

  implicitWidth: Style.space(76)
  implicitHeight: ring.height + captionText.implicitHeight + Style.space(4)

  readonly property color track: Qt.rgba(ink.r, ink.g, ink.b, 0.16)
  property real shown: remaining < 0 ? 0 : remaining

  Behavior on shown {
    NumberAnimation { duration: Style.duration(180); easing.type: Easing.OutCubic }
  }

  Canvas {
    id: ring
    width: Style.space(56)
    height: width
    anchors.horizontalCenter: parent.horizontalCenter
    anchors.top: parent.top
    onPaint: {
      var ctx = getContext("2d")
      var d = width
      var stroke = Math.max(3, Style.space(4))
      var radius = (d - stroke) / 2
      ctx.reset()
      ctx.lineWidth = stroke
      ctx.lineCap = "round"
      ctx.strokeStyle = root.track
      ctx.beginPath()
      ctx.arc(d / 2, d / 2, radius, 0, Math.PI * 2)
      ctx.stroke()
      if (root.remaining >= 0 && root.shown > 0) {
        ctx.strokeStyle = root.ink
        ctx.beginPath()
        var start = -Math.PI / 2
        var sweep = Math.PI * 2 * Math.max(0.015, Math.min(1, root.shown / 100))
        ctx.arc(d / 2, d / 2, radius, start, start + sweep, false)
        ctx.stroke()
      }
    }
  }

  onShownChanged: ring.requestPaint()
  onRemainingChanged: ring.requestPaint()
  onInkChanged: ring.requestPaint()
  onTrackChanged: ring.requestPaint()

  Rectangle {
    anchors.fill: ring
    radius: width / 2
    color: "transparent"
    border.width: root.hasCursor ? Math.max(1, Style.space(1)) : 0
    border.color: root.ink
  }

  Text {
    anchors.centerIn: ring
    textFormat: Text.PlainText
    text: root.confirmed ? "✓" : (root.remaining < 0 ? "–" : Commons.I18n.formatNumber(Math.round(root.shown), 0))
    color: root.ink
    font.family: root.fontFamily
    font.pixelSize: Style.font.body
    font.bold: true
  }

  Text {
    id: captionText
    anchors.top: ring.bottom
    anchors.topMargin: Style.space(4)
    anchors.horizontalCenter: parent.horizontalCenter
    width: parent.width
    horizontalAlignment: Text.AlignHCenter
    elide: Text.ElideRight
    textFormat: Text.PlainText
    text: root.caption
    color: Qt.darker(root.ink, 1.35)
    font.family: root.fontFamily
    font.pixelSize: Style.font.caption
  }

  MouseArea {
    id: mouse
    anchors.fill: parent
    hoverEnabled: true
    cursorShape: Qt.PointingHandCursor
    onClicked: root.clicked()
  }

  PanelToolTip {
    visible: mouse.containsMouse && root.tip !== ""
    text: root.tip
    fontFamily: root.fontFamily
  }
}
