import QtQuick
import qs.Commons
import qs.Commons as Commons

// Small status label ("In Factory", banked-reset count). One shape for every
// tag in the panel: the shell corner radius, caption text, accent fill.
Rectangle {
  id: root

  property string text: ""
  property bool bold: false
  property string fontFamily: Style.font.family
  property color textColor: Commons.Color.foreground

  radius: Style.cornerRadius
  color: Style.selectedAccentFill
  implicitWidth: label.implicitWidth + Style.space(16)
  implicitHeight: label.implicitHeight + Style.space(6)

  Text {
    id: label
    anchors.centerIn: parent
    textFormat: Text.PlainText
    text: root.text
    color: root.textColor
    font.family: root.fontFamily
    font.pixelSize: Style.font.caption
    font.bold: root.bold
  }
}
