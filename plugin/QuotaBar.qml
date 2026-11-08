import QtQuick
import QtQuick.Layouts
import qs.Commons
import qs.Commons as Commons
import qs.Ui

// Remaining quota as a horizontal bar, following the Agents panel Meter:
// a track in the shell corner radius, a fill that moves only when a new
// reading arrives, and the percentage as plain tabular text. No ring.
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
  signal hovered(bool on)

  readonly property real thickness: Math.max(Style.space(4), Math.round(Style.spacing.controlHeight * 0.14))
  readonly property color track: Qt.rgba(ink.r, ink.g, ink.b, 0.16)
  property real shown: remaining < 0 ? 0 : remaining

  implicitHeight: barColumn.implicitHeight

  Behavior on shown {
    NumberAnimation { duration: Style.duration(180); easing.type: Easing.OutCubic }
  }

  Rectangle {
    anchors.fill: parent
    anchors.margins: -Style.space(3)
    radius: Style.cornerRadius
    color: "transparent"
    border.width: root.hasCursor ? Math.max(1, Style.space(1)) : 0
    border.color: root.ink
  }

  Column {
    id: barColumn
    anchors.left: parent.left
    anchors.right: parent.right
    anchors.top: parent.top
    spacing: Style.space(3)

    RowLayout {
      width: parent.width
      spacing: Style.space(8)

      Text {
        Layout.fillWidth: true
        textFormat: Text.PlainText
        elide: Text.ElideRight
        text: root.tip !== "" ? root.caption + " · " + root.tip : root.caption
        color: Qt.darker(root.ink, 1.2)
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
      }
      Text {
        textFormat: Text.PlainText
        text: root.confirmed ? "✓" : (root.remaining < 0 ? "–" : Commons.I18n.formatNumber(Math.round(root.shown), 0) + "%")
        color: root.ink
        font.family: root.fontFamily
        font.pixelSize: Style.font.caption
        font.bold: true
      }
    }

    Rectangle {
      id: trackRect
      width: parent.width
      height: root.thickness
      radius: Math.min(Style.cornerRadius, height / 2)
      color: root.track

      Rectangle {
        anchors.left: parent.left
        anchors.verticalCenter: parent.verticalCenter
        height: parent.height
        radius: parent.radius
        width: parent.width * Math.max(0, Math.min(1, root.shown / 100))
        color: root.ink

        Behavior on width {
          NumberAnimation { duration: Style.duration(160); easing.type: Easing.OutCubic }
        }
      }
    }
  }

  MouseArea {
    id: mouse
    anchors.fill: parent
    hoverEnabled: true
    cursorShape: Qt.PointingHandCursor
    onEntered: root.hovered(true)
    onExited: root.hovered(false)
    onClicked: root.clicked()
  }

  PanelToolTip {
    visible: mouse.containsMouse && root.tip !== ""
    text: root.tip
    fontFamily: root.fontFamily
  }
}
