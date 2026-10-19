package blueprint

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

const testBlueprint = `name: wordpress
description: WordPress with MySQL
version: "1.0"
parameters:
  - name: projectName
    description: Project name
    required: true
  - name: storageSize
    description: Upload storage size
    default: "5Gi"
---
project: "{{ .projectName }}"

apps:
  wordpress:
    image: wordpress:6-apache
    port: 80
    serviceBindings:
      - name: db
        prefix: WORDPRESS_DB_

services:
  db:
    type: mysql
    storage: 5Gi

volumes:
  uploads:
    size: "{{ .storageSize }}"
    mounts:
      - app: wordpress
        mountPath: /var/www/html/wp-content/uploads
`

func TestParse(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	assert.Equal(t, "wordpress", bp.Metadata.Name)
	assert.Equal(t, "WordPress with MySQL", bp.Metadata.Description)
	assert.Equal(t, "1.0", bp.Metadata.Version)
	require.Len(t, bp.Metadata.Parameters, 2)
	assert.True(t, bp.Metadata.Parameters[0].Required)
	assert.Equal(t, "5Gi", bp.Metadata.Parameters[1].Default)
	assert.Contains(t, bp.Template, "{{ .projectName }}")
}

func TestParse_MissingName(t *testing.T) {
	_, err := Parse([]byte("description: test\n---\nproject: x"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must have a name")
}

func TestParse_SingleDocument(t *testing.T) {
	_, err := Parse([]byte("name: test\nproject: x"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "two YAML documents")
}

func TestRender(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "my-blog"})
	require.NoError(t, err)

	assert.Equal(t, "my-blog", m.Project)
	require.Contains(t, m.Apps, "wordpress")
	assert.Equal(t, int32(80), m.Apps["wordpress"].Port)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "mysql", m.Services["db"].Type)
	require.Contains(t, m.Volumes, "uploads")
	assert.Equal(t, "5Gi", m.Volumes["uploads"].Size) // default applied
}

func TestRender_CustomParameter(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "my-blog",
		"storageSize": "20Gi",
	})
	require.NoError(t, err)

	assert.Equal(t, "20Gi", m.Volumes["uploads"].Size)
}

func TestRender_MissingRequired(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	_, err = bp.Render(map[string]string{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "projectName")
}

func TestRenderYAML(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	data, err := bp.RenderYAML(map[string]string{"projectName": "my-blog"})
	require.NoError(t, err)

	assert.Contains(t, string(data), "project: \"my-blog\"")
	assert.Contains(t, string(data), "size: \"5Gi\"")
	assert.NotContains(t, string(data), "{{")
}

func TestRenderYAML_MissingRequired(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	_, err = bp.RenderYAML(map[string]string{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "projectName")
}

func TestRender_ServiceBindings(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "test"})
	require.NoError(t, err)

	require.Len(t, m.Apps["wordpress"].ServiceBindings, 1)
	assert.Equal(t, "db", m.Apps["wordpress"].ServiceBindings[0].Name)
	assert.Equal(t, "WORDPRESS_DB_", m.Apps["wordpress"].ServiceBindings[0].Prefix)
}

func TestRender_VolumeMounts(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "test"})
	require.NoError(t, err)

	require.Len(t, m.Volumes["uploads"].Mounts, 1)
	assert.Equal(t, "wordpress", m.Volumes["uploads"].Mounts[0].App)
	assert.Equal(t, "/var/www/html/wp-content/uploads", m.Volumes["uploads"].Mounts[0].MountPath)
}

func TestRender_DefaultOverride(t *testing.T) {
	bp, err := Parse([]byte(testBlueprint))
	require.NoError(t, err)

	// storageSize has default "5Gi", override it
	m, err := bp.Render(map[string]string{"projectName": "test", "storageSize": "50Gi"})
	require.NoError(t, err)

	assert.Equal(t, "50Gi", m.Volumes["uploads"].Size)
}

func TestRender_InvalidTemplate(t *testing.T) {
	data := []byte("name: bad\n---\nproject: \"{{ .missing }}\"")
	bp, err := Parse(data)
	require.NoError(t, err)

	_, err = bp.Render(map[string]string{})
	assert.Error(t, err)
}

func TestParse_EmptyMetadata(t *testing.T) {
	_, err := Parse([]byte("---\nproject: test"))
	assert.Error(t, err)
}

func TestParse_EmptyTemplate(t *testing.T) {
	_, err := Parse([]byte("name: test\n---\n"))
	// Should succeed — empty template is valid YAML (nil manifest)
	// but will fail validation when rendered
	assert.NoError(t, err)
}

func TestSplitDocuments(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected int
	}{
		{"two documents", "a: 1\n---\nb: 2", 2},
		{"three documents", "a: 1\n---\nb: 2\n---\nc: 3", 3},
		{"no separator", "a: 1\nb: 2", 1},
		{"leading separator", "---\na: 1\n---\nb: 2", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := splitDocuments(tt.input)
			assert.Len(t, parts, tt.expected)
		})
	}
}

// Registry tests

func TestList(t *testing.T) {
	blueprints, err := List()
	require.NoError(t, err)

	assert.GreaterOrEqual(t, len(blueprints), 12)

	names := make(map[string]bool)
	for _, bp := range blueprints {
		names[bp.Name] = true
		assert.NotEmpty(t, bp.Description)
		assert.NotEmpty(t, bp.Version)
	}

	for _, expected := range []string{
		"wordpress", "ghost", "gitea", "plausible",
		"medusa", "n8n", "uptime-kuma", "outline",
		"calcom", "invoice-ninja", "mattermost", "rocketchat",
	} {
		assert.True(t, names[expected], "missing blueprint: %s", expected)
	}
}

func TestList_Sorted(t *testing.T) {
	blueprints, err := List()
	require.NoError(t, err)

	for i := 1; i < len(blueprints); i++ {
		assert.True(t, blueprints[i-1].Name <= blueprints[i].Name,
			"expected %s <= %s", blueprints[i-1].Name, blueprints[i].Name)
	}
}

func TestGet_Existing(t *testing.T) {
	bp, err := Get("wordpress")
	require.NoError(t, err)

	assert.Equal(t, "wordpress", bp.Metadata.Name)
	assert.NotEmpty(t, bp.Template)
}

func TestGet_NotFound(t *testing.T) {
	_, err := Get("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// Built-in blueprint rendering tests

func TestBuiltinWordpress(t *testing.T) {
	bp, err := Get("wordpress")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "blog"})
	require.NoError(t, err)

	assert.Equal(t, "blog", m.Project)
	require.Contains(t, m.Apps, "wordpress")
	assert.Equal(t, int32(80), m.Apps["wordpress"].Port)
	assert.Equal(t, "wordpress:6-apache", m.Apps["wordpress"].Image)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "mysql", m.Services["db"].Type)
	require.Contains(t, m.Volumes, "uploads")
}

func TestBuiltinGhost(t *testing.T) {
	bp, err := Get("ghost")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "site", "domain": "https://blog.example.com"})
	require.NoError(t, err)

	assert.Equal(t, "site", m.Project)
	require.Contains(t, m.Apps, "ghost")
	assert.Equal(t, int32(2368), m.Apps["ghost"].Port)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "mysql", m.Services["db"].Type)
}

func TestBuiltinGitea(t *testing.T) {
	bp, err := Get("gitea")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "code"})
	require.NoError(t, err)

	assert.Equal(t, "code", m.Project)
	require.Contains(t, m.Apps, "gitea")
	assert.Equal(t, int32(3000), m.Apps["gitea"].Port)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "postgres", m.Services["db"].Type)
	require.Contains(t, m.Volumes, "repos")
}

func TestBuiltinPlausible(t *testing.T) {
	bp, err := Get("plausible")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "analytics",
		"baseUrl":     "https://analytics.example.com",
	})
	require.NoError(t, err)

	assert.Equal(t, "analytics", m.Project)
	require.Contains(t, m.Apps, "plausible")
	assert.Equal(t, int32(8000), m.Apps["plausible"].Port)
	assert.Contains(t, m.Apps["plausible"].Env["BASE_URL"], "https://analytics.example.com")
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "postgres", m.Services["db"].Type)
}

func TestBuiltinPlausible_MissingRequiredBaseUrl(t *testing.T) {
	bp, err := Get("plausible")
	require.NoError(t, err)

	_, err = bp.Render(map[string]string{"projectName": "analytics"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "baseUrl")
}

func TestBuiltinMedusa(t *testing.T) {
	bp, err := Get("medusa")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "shop"})
	require.NoError(t, err)

	assert.Equal(t, "shop", m.Project)
	require.Contains(t, m.Apps, "medusa")
	assert.Equal(t, int32(9000), m.Apps["medusa"].Port)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "postgres", m.Services["db"].Type)
	require.Contains(t, m.Services, "redis")
	assert.Equal(t, "redis", m.Services["redis"].Type)
}

func TestBuiltinN8n(t *testing.T) {
	bp, err := Get("n8n")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "automation"})
	require.NoError(t, err)

	assert.Equal(t, "automation", m.Project)
	require.Contains(t, m.Apps, "n8n")
	assert.Equal(t, int32(5678), m.Apps["n8n"].Port)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "postgres", m.Services["db"].Type)
}

func TestBuiltinUptimeKuma(t *testing.T) {
	bp, err := Get("uptime-kuma")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{"projectName": "monitoring"})
	require.NoError(t, err)

	assert.Equal(t, "monitoring", m.Project)
	require.Contains(t, m.Apps, "uptime-kuma")
	assert.Equal(t, int32(3001), m.Apps["uptime-kuma"].Port)
	assert.Empty(t, m.Services) // no database needed
	require.Contains(t, m.Volumes, "data")
}

func TestBuiltinOutline(t *testing.T) {
	bp, err := Get("outline")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "wiki",
		"domain":      "https://wiki.example.com",
	})
	require.NoError(t, err)

	assert.Equal(t, "wiki", m.Project)
	require.Contains(t, m.Apps, "outline")
	assert.Equal(t, int32(3000), m.Apps["outline"].Port)
	assert.Contains(t, m.Apps["outline"].Env["URL"], "https://wiki.example.com")
	require.Contains(t, m.Services, "db")
	require.Contains(t, m.Services, "redis")
	require.Contains(t, m.Services, "minio")
}

func TestBuiltinOutline_MissingDomain(t *testing.T) {
	bp, err := Get("outline")
	require.NoError(t, err)

	_, err = bp.Render(map[string]string{"projectName": "wiki"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "domain")
}

func TestBuiltinCalcom(t *testing.T) {
	bp, err := Get("calcom")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "scheduling",
		"domain":      "https://cal.example.com",
	})
	require.NoError(t, err)

	assert.Equal(t, "scheduling", m.Project)
	require.Contains(t, m.Apps, "calcom")
	assert.Equal(t, int32(3000), m.Apps["calcom"].Port)
	assert.Contains(t, m.Apps["calcom"].Env["NEXT_PUBLIC_WEBAPP_URL"], "https://cal.example.com")
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "postgres", m.Services["db"].Type)
}

func TestBuiltinInvoiceNinja(t *testing.T) {
	bp, err := Get("invoice-ninja")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "billing",
		"domain":      "https://invoices.example.com",
	})
	require.NoError(t, err)

	assert.Equal(t, "billing", m.Project)
	require.Contains(t, m.Apps, "invoice-ninja")
	assert.Equal(t, int32(9000), m.Apps["invoice-ninja"].Port)
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "mysql", m.Services["db"].Type)
	require.Contains(t, m.Volumes, "storage")
}

func TestBuiltinMattermost(t *testing.T) {
	bp, err := Get("mattermost")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "chat",
		"domain":      "https://chat.example.com",
	})
	require.NoError(t, err)

	assert.Equal(t, "chat", m.Project)
	require.Contains(t, m.Apps, "mattermost")
	assert.Equal(t, int32(8065), m.Apps["mattermost"].Port)
	assert.Contains(t, m.Apps["mattermost"].Env["MM_SERVICESETTINGS_SITEURL"], "https://chat.example.com")
	require.Contains(t, m.Services, "db")
	assert.Equal(t, "postgres", m.Services["db"].Type)
	require.Contains(t, m.Volumes, "data")
}

func TestBuiltinRocketchat(t *testing.T) {
	bp, err := Get("rocketchat")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "team",
		"domain":      "https://chat.example.com",
	})
	require.NoError(t, err)

	assert.Equal(t, "team", m.Project)
	require.Contains(t, m.Apps, "rocketchat")
	assert.Equal(t, int32(3000), m.Apps["rocketchat"].Port)
	assert.Equal(t, "rocket.chat:8.4.3", m.Apps["rocketchat"].Image)
	assert.Contains(t, m.Apps["rocketchat"].Env["ROOT_URL"], "https://chat.example.com")
	require.Contains(t, m.Services, "mongodb")
	assert.Equal(t, "mongodb", m.Services["mongodb"].Type)
}

func TestBuiltinMattermost_SMTP(t *testing.T) {
	bp, err := Get("mattermost")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName":  "chat",
		"domain":       "https://chat.example.com",
		"smtpHost":     "smtp.example.com",
		"smtpUsername": "postmaster@example.com",
		"smtpPassword": "s3cret",
		"smtpFrom":     "notifications@example.com",
	})
	require.NoError(t, err)

	env := m.Apps["mattermost"].Env
	assert.Equal(t, "smtp.example.com", env["MM_EMAILSETTINGS_SMTPSERVER"])
	assert.Equal(t, "587", env["MM_EMAILSETTINGS_SMTPPORT"])
	assert.Equal(t, "STARTTLS", env["MM_EMAILSETTINGS_CONNECTIONSECURITY"])
	assert.Equal(t, "true", env["MM_EMAILSETTINGS_ENABLESMTPAUTH"])
	assert.Equal(t, "true", env["MM_EMAILSETTINGS_SENDEMAILNOTIFICATIONS"])
	assert.Equal(t, "postmaster@example.com", env["MM_EMAILSETTINGS_SMTPUSERNAME"])
	assert.Equal(t, "s3cret", env["MM_EMAILSETTINGS_SMTPPASSWORD"])
	assert.Equal(t, "notifications@example.com", env["MM_EMAILSETTINGS_FEEDBACKEMAIL"])
	assert.Equal(t, "notifications@example.com", env["MM_EMAILSETTINGS_REPLYTOADDRESS"])
}

func TestBuiltinMattermost_SMTPWithoutAuth(t *testing.T) {
	bp, err := Get("mattermost")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName":  "chat",
		"domain":       "https://chat.example.com",
		"smtpHost":     "relay.internal",
		"smtpSecurity": "none",
	})
	require.NoError(t, err)

	env := m.Apps["mattermost"].Env
	assert.Equal(t, "false", env["MM_EMAILSETTINGS_ENABLESMTPAUTH"])
	assert.Equal(t, "", env["MM_EMAILSETTINGS_CONNECTIONSECURITY"])
	_, hasUser := env["MM_EMAILSETTINGS_SMTPUSERNAME"]
	assert.False(t, hasUser)
}

func TestBuiltinMattermost_NoNotificationsByDefault(t *testing.T) {
	bp, err := Get("mattermost")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "chat",
		"domain":      "https://chat.example.com",
	})
	require.NoError(t, err)

	env := m.Apps["mattermost"].Env
	_, hasSMTP := env["MM_EMAILSETTINGS_SMTPSERVER"]
	assert.False(t, hasSMTP)
	_, hasPush := env["MM_EMAILSETTINGS_PUSHNOTIFICATIONSERVER"]
	assert.False(t, hasPush)
}

func TestBuiltinMattermost_Push(t *testing.T) {
	bp, err := Get("mattermost")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName":            "chat",
		"domain":                 "https://chat.example.com",
		"pushNotificationServer": "https://push-test.mattermost.com",
	})
	require.NoError(t, err)

	env := m.Apps["mattermost"].Env
	assert.Equal(t, "true", env["MM_EMAILSETTINGS_SENDPUSHNOTIFICATIONS"])
	assert.Equal(t, "https://push-test.mattermost.com", env["MM_EMAILSETTINGS_PUSHNOTIFICATIONSERVER"])
}

func TestBuiltinRocketchat_SMTP(t *testing.T) {
	bp, err := Get("rocketchat")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName":  "team",
		"domain":       "https://chat.example.com",
		"smtpHost":     "smtp.example.com",
		"smtpUsername": "postmaster@example.com",
		"smtpPassword": "s3cret",
		"smtpFrom":     "notifications@example.com",
	})
	require.NoError(t, err)

	env := m.Apps["rocketchat"].Env
	assert.Equal(t, "smtp.example.com", env["OVERWRITE_SETTING_SMTP_Host"])
	assert.Equal(t, "587", env["OVERWRITE_SETTING_SMTP_Port"])
	assert.Equal(t, "smtp", env["OVERWRITE_SETTING_SMTP_Protocol"])
	assert.Equal(t, "false", env["OVERWRITE_SETTING_SMTP_IgnoreTLS"])
	assert.Equal(t, "postmaster@example.com", env["OVERWRITE_SETTING_SMTP_Username"])
	assert.Equal(t, "s3cret", env["OVERWRITE_SETTING_SMTP_Password"])
	assert.Equal(t, "notifications@example.com", env["OVERWRITE_SETTING_From_Email"])
}

func TestBuiltinRocketchat_SMTPImplicitTLS(t *testing.T) {
	bp, err := Get("rocketchat")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName":  "team",
		"domain":       "https://chat.example.com",
		"smtpHost":     "smtp.example.com",
		"smtpPort":     "465",
		"smtpSecurity": "TLS",
	})
	require.NoError(t, err)

	env := m.Apps["rocketchat"].Env
	assert.Equal(t, "smtps", env["OVERWRITE_SETTING_SMTP_Protocol"])
	assert.Equal(t, "true", env["OVERWRITE_SETTING_SMTP_IgnoreTLS"])
}

func TestBuiltinRocketchat_NoSMTPByDefault(t *testing.T) {
	bp, err := Get("rocketchat")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "team",
		"domain":      "https://chat.example.com",
	})
	require.NoError(t, err)

	env := m.Apps["rocketchat"].Env
	_, ok := env["OVERWRITE_SETTING_SMTP_Host"]
	assert.False(t, ok)
}

func TestBuiltinWordpress_WithEnvironment(t *testing.T) {
	bp, err := Get("wordpress")
	require.NoError(t, err)

	m, err := bp.Render(map[string]string{
		"projectName": "blog",
		"environment": "prod",
	})
	require.NoError(t, err)

	assert.Equal(t, "prod", m.Environment)
	assert.Contains(t, m.Apps["wordpress"].Env["WORDPRESS_DB_HOST"], "blog-prod")
}

func TestBuiltinBlueprintsRenderValidRouteHosts(t *testing.T) {
	metas, err := List()
	require.NoError(t, err)
	require.NotEmpty(t, metas)
	for _, meta := range metas {
		for _, domain := range []string{"https://app.example.com", "app.example.com"} {
			t.Run(meta.Name+"/"+domain, func(t *testing.T) {
				b, err := Get(meta.Name)
				require.NoError(t, err)
				params := map[string]string{"domain": domain, "baseUrl": "https://app.example.com"}
				for _, p := range b.Metadata.Parameters {
					if p.Required && params[p.Name] == "" {
						params[p.Name] = "shop"
					}
				}
				m, err := b.Render(params)
				require.NoError(t, err)
				routed := 0
				for name, app := range m.Apps {
					if app.Route == nil {
						continue
					}
					routed++
					assert.NoError(t, manifest.ValidateRouteHost(app.Route.Host), "app %s", name)
					assert.Equal(t, "app.example.com", app.Route.Host, "app %s", name)
				}
				assert.NotZero(t, routed, "every built-in blueprint publishes a route")
			})
		}
	}
}

func TestBuiltinBlueprintsPassAFullURLToTheApp(t *testing.T) {
	urlEnv := map[string][2]string{
		"calcom":        {"calcom", "NEXT_PUBLIC_WEBAPP_URL"},
		"calcom-auth":   {"calcom", "NEXTAUTH_URL"},
		"ghost":         {"ghost", "url"},
		"invoice-ninja": {"invoice-ninja", "APP_URL"},
		"mattermost":    {"mattermost", "MM_SERVICESETTINGS_SITEURL"},
		"outline":       {"outline", "URL"},
		"rocketchat":    {"rocketchat", "ROOT_URL"},
	}
	for name, where := range urlEnv {
		for _, domain := range []string{"https://app.example.com", "app.example.com"} {
			t.Run(name+"/"+domain, func(t *testing.T) {
				b, err := Get(strings.TrimSuffix(name, "-auth"))
				require.NoError(t, err)
				m, err := b.Render(map[string]string{"projectName": "shop", "domain": domain})
				require.NoError(t, err)
				app, ok := m.Apps[where[0]]
				require.True(t, ok, "blueprint %s has no app %s", name, where[0])
				assert.Equal(t, "https://app.example.com", app.Env[where[1]])
			})
		}
	}
}

func TestBuiltinBlueprintsWithoutADomainUseTheGeneratedHost(t *testing.T) {
	for _, name := range []string{"gitea", "medusa", "n8n", "plausible", "uptime-kuma", "wordpress"} {
		b, err := Get(name)
		require.NoError(t, err)
		m, err := b.Render(map[string]string{"projectName": "shop", "baseUrl": "https://app.example.com"})
		require.NoError(t, err, name)
		for app, spec := range m.Apps {
			if spec.Route != nil {
				assert.Empty(t, spec.Route.Host, "%s/%s", name, app)
			}
		}
	}
}

func TestGhostRequiresItsPublicURL(t *testing.T) {
	b, err := Get("ghost")
	require.NoError(t, err)
	_, err = b.Render(map[string]string{"projectName": "shop"})
	assert.ErrorContains(t, err, "domain")
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://Shop.Example.com/":     "shop.example.com",
		"http://shop.example.com:8080":  "shop.example.com",
		"https://shop.example.com/blog": "shop.example.com",
		"shop.example.com":              "shop.example.com",
		"":                              "",
	} {
		got, err := hostOf(in)
		assert.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
}

func TestURLOf(t *testing.T) {
	assert.Equal(t, "https://shop.example.com", urlOf("shop.example.com"))
	assert.Equal(t, "http://shop.example.com:8080", urlOf("http://shop.example.com:8080"))
	assert.Equal(t, "", urlOf(""))
}

func TestBlueprintsRefuseAMalformedDomain(t *testing.T) {
	for _, domain := range []string{"https://blog.example.com:bad", "https:///blog.example.com", "blog example.com"} {
		b, err := Get("ghost")
		require.NoError(t, err)
		_, err = b.Render(map[string]string{"projectName": "shop", "domain": domain})
		assert.ErrorContains(t, err, "is not a valid public address", domain)
		_, err = b.RenderYAML(map[string]string{"projectName": "shop", "domain": domain})
		assert.ErrorContains(t, err, "is not a valid public address", domain)
	}
}

func TestBlueprintsRefuseABlankRequiredParameter(t *testing.T) {
	b, err := Get("ghost")
	require.NoError(t, err)
	_, err = b.Render(map[string]string{"projectName": "shop", "domain": "   "})
	assert.ErrorContains(t, err, `required parameter "domain" is missing`)
	_, err = b.RenderYAML(map[string]string{"projectName": "shop", "domain": "   "})
	assert.ErrorContains(t, err, `required parameter "domain" is missing`)
}

func TestInstallCommandRendersForEveryBuiltin(t *testing.T) {
	metas, err := List()
	require.NoError(t, err)
	for _, meta := range metas {
		cmd := InstallCommand(meta)
		assert.True(t, strings.HasPrefix(cmd, "kip blueprint install "+meta.Name+" "), cmd)
		params := map[string]string{}
		for _, field := range strings.Fields(cmd) {
			if k, v, ok := strings.Cut(field, "="); ok {
				params[k] = v
			}
		}
		b, err := Get(meta.Name)
		require.NoError(t, err)
		_, err = b.Render(params)
		assert.NoError(t, err, cmd)
	}
	ghost, err := Get("ghost")
	require.NoError(t, err)
	assert.Contains(t, InstallCommand(ghost.Metadata), "--set domain=https://app.example.com")
}

func TestBuiltinPostgresBlueprintsKeepAcceptingPlaintext(t *testing.T) {
	metas, err := List()
	require.NoError(t, err)
	checked := 0
	for _, meta := range metas {
		b, err := Get(meta.Name)
		require.NoError(t, err)
		params := map[string]string{}
		for _, p := range b.Metadata.Parameters {
			params[p.Name] = "x"
		}
		params["domain"] = "app.example.com"
		m, err := b.Render(params)
		require.NoError(t, err, meta.Name)
		for name, svc := range m.Services {
			if svc.Type == "postgres" {
				checked++
				assert.Equal(t, "on", svc.TLS, "%s/%s: its app is set up without TLS, so a required default would cut it off", meta.Name, name)
			}
		}
	}
	assert.GreaterOrEqual(t, checked, 7, "every built-in blueprint with Postgres was rendered")
}
