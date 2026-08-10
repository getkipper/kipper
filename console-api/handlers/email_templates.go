package handlers

import "fmt"

func inviteEmailHTML(inviteURL, role, expiresIn, clusterDomain string) string {
	roleColor := "#0284c7" // kipper-600
	switch role {
	case "admin":
		roleColor = "#dc2626"
	case "viewer":
		roleColor = "#64748b"
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><meta charset="UTF-8"></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#f0f9ff;">
  <table width="100%%" cellpadding="0" cellspacing="0" style="background:#f0f9ff;min-height:100vh;">
    <tr><td align="center" style="padding:48px 16px;">
      <div style="max-width:480px;background:#fff;border-radius:12px;border:1px solid #e2e8f0;overflow:hidden;">
        <div style="padding:32px 32px 0;">
          <h1 style="margin:0 0 8px;font-size:20px;font-weight:600;color:#0f172a;">You're invited to join a cluster</h1>
          <p style="margin:0 0 24px;font-size:14px;color:#64748b;">
            You've been invited as
            <span style="display:inline-block;padding:2px 8px;border-radius:4px;font-size:12px;font-weight:600;color:#fff;background:%s;">%s</span>
            on <strong>%s</strong>
          </p>
        </div>
        <div style="padding:0 32px 32px;text-align:center;">
          <a href="%s" style="display:inline-block;padding:12px 32px;background:#0284c7;color:#fff;text-decoration:none;border-radius:8px;font-size:14px;font-weight:600;">Accept Invite</a>
          <p style="margin:16px 0 0;font-size:12px;color:#94a3b8;">This link expires in %s.</p>
        </div>
        <div style="padding:16px 32px;background:#f0f9ff;border-top:1px solid #e2e8f0;">
          <p style="margin:0;font-size:11px;color:#94a3b8;text-align:center;">Sent by Kipper on %s</p>
        </div>
      </div>
    </td></tr>
  </table>
</body>
</html>`, roleColor, role, clusterDomain, inviteURL, expiresIn, clusterDomain)
}
