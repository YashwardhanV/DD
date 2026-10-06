package agent

import "strings"

// powershellAppID is the notification identity of Windows PowerShell. Windows
// only shows a toast for a registered app, and borrowing PowerShell's means we
// need no installer or shortcut.
const powershellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

// psQuote makes text safe inside a PowerShell single-quoted string (where the
// only special character is the quote itself, written twice).
func psQuote(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ToastScript builds the PowerShell commands that show a Windows notification
// with the given title and body. The text is passed as quoted strings, never
// pasted into code, so a drive called  '); Remove-Item ...  can't run anything.
func ToastScript(title, body string) string {
	return strings.Join([]string{
		`[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null`,
		`$t = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02)`,
		`$n = $t.GetElementsByTagName('text')`,
		`$n.Item(0).AppendChild($t.CreateTextNode(` + psQuote(title) + `)) | Out-Null`,
		`$n.Item(1).AppendChild($t.CreateTextNode(` + psQuote(body) + `)) | Out-Null`,
		`$toast = [Windows.UI.Notifications.ToastNotification]::new($t)`,
		`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier(` + psQuote(powershellAppID) + `).Show($toast)`,
	}, "; ")
}
