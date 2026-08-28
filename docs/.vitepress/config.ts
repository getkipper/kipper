import { defineConfig, type HeadConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'

const SITE_URL = 'https://docs.getkipper.com'
const SITE_TITLE = 'Kipper'
const SITE_DESCRIPTION = 'Turn a plain Linux server into production Kubernetes in one command: web console, automatic SSL, storage and backups. Open source, self-hosted.'

export default withMermaid(
  defineConfig({
    title: SITE_TITLE,
    description: SITE_DESCRIPTION,

    // Held back from the build. A third review found the recovery still prints
    // success after a failed patch, and that restoring only the certificate
    // yields a certificate and key that do not match when the live key has
    // moved — which breaks the fingerprint the gateway pins, the exact hazard
    // the rewrite was meant to remove. The procedure and its crypto are sound;
    // the recovery is not, and that is the half people reach for when the rest
    // has gone wrong.
    srcExclude: ['en/certificate-authority.md'],

    sitemap: { hostname: SITE_URL },

    // VitePress emits one static head for the whole site, so without this every
    // page would share the home page's URL and title.
    transformHead({ pageData }) {
      const title = pageData.title ? `${pageData.title} | ${SITE_TITLE}` : SITE_TITLE
      const head: HeadConfig[] = [
        ['meta', { property: 'og:title', content: title }],
        ['meta', { name: 'twitter:title', content: title }],
      ]

      // The generated 404 answers on any URL, so it claims none of them.
      if (pageData.relativePath !== '404.md') {
        const path = pageData.relativePath
          .replace(/index\.md$/, '')
          .replace(/\.md$/, '.html')
        head.push(
          ['link', { rel: 'canonical', href: `${SITE_URL}/${path}` }],
          ['meta', { property: 'og:url', content: `${SITE_URL}/${path}` }],
        )
      }

      return head
    },

    head: [
      ['link', { rel: 'icon', type: 'image/svg+xml', href: '/logo.svg' }],
      ['link', { rel: 'alternate icon', type: 'image/x-icon', href: '/favicon.ico' }],
      ['link', { rel: 'apple-touch-icon', sizes: '180x180', href: '/apple-touch-icon.png' }],
      ['link', { rel: 'icon', type: 'image/png', sizes: '192x192', href: '/icon-192.png' }],
      ['link', { rel: 'icon', type: 'image/png', sizes: '512x512', href: '/icon-512.png' }],
      ['link', { rel: 'manifest', href: '/site.webmanifest' }],
      ['meta', { name: 'theme-color', content: '#0ea5e9' }],
      ['meta', { name: 'application-name', content: SITE_TITLE }],
      ['meta', { name: 'apple-mobile-web-app-title', content: SITE_TITLE }],
      ['meta', { name: 'apple-mobile-web-app-capable', content: 'yes' }],
      ['meta', { name: 'apple-mobile-web-app-status-bar-style', content: 'default' }],
      ['meta', { property: 'og:type', content: 'website' }],
      ['meta', { property: 'og:site_name', content: SITE_TITLE }],
      ['meta', { property: 'og:description', content: SITE_DESCRIPTION }],
      ['meta', { property: 'og:image', content: `${SITE_URL}/og-image.png` }],
      ['meta', { property: 'og:image:width', content: '1200' }],
      ['meta', { property: 'og:image:height', content: '630' }],
      ['meta', { property: 'og:image:alt', content: `${SITE_TITLE}: ${SITE_DESCRIPTION}` }],
      ['meta', { name: 'twitter:card', content: 'summary_large_image' }],
      ['meta', { name: 'twitter:description', content: SITE_DESCRIPTION }],
      ['meta', { name: 'twitter:image', content: `${SITE_URL}/og-image.png` }],
      // Plausible sets no cookies and stores no personal data, so the site
      // needs no consent banner. plausible.io is on the app's CSP allowlist.
      // The script filename identifies the site, so there is no data-domain.
      ['script', { async: '', src: 'https://plausible.io/js/pa-hNLTVzwL4CpsYcJEEtoFW.js' }],
      ['script', {}, `window.plausible=window.plausible||function(){(plausible.q=plausible.q||[]).push(arguments)},plausible.init=plausible.init||function(i){plausible.o=i||{}};plausible.init()`],
    ],

    locales: {
      en: {
        label: 'English',
        lang: 'en',
        link: '/en/',
      },
    },

    themeConfig: {
      logo: '/logo.svg',

      nav: [
        { text: 'Guide', link: '/en/getting-started' },
        { text: 'Reference', link: '/en/installation' },
        { text: 'Architecture', link: '/en/architecture' },
      ],

      sidebar: {
        '/en/': [
          {
            text: 'Getting Started',
            items: [
              { text: 'Overview', link: '/en/' },
              { text: 'Getting Started', link: '/en/getting-started' },
              { text: 'Installation', link: '/en/installation' },
            ],
          },
          {
            text: 'Using Kipper',
            items: [
              { text: 'Deploying Apps', link: '/en/deploying-apps' },
              { text: 'Stateful Services', link: '/en/services' },
              { text: 'Database Console', link: '/en/database-console' },
              { text: 'Shared Storage', link: '/en/shared-storage' },
              { text: 'Projects & Environments', link: '/en/environments' },
              { text: 'Functions (Serverless)', link: '/en/functions' },
              { text: 'Jobs & Scheduled Tasks', link: '/en/jobs' },
              { text: 'Secrets & Environment', link: '/en/secrets' },
              { text: 'Webhooks & CI/CD', link: '/en/webhooks' },
              { text: 'Domains & SSL', link: '/en/domains' },
              { text: 'URL Redirects', link: '/en/redirects' },
              { text: 'API Keys & Usage Plans', link: '/en/api-keys' },
              { text: 'Browsing Files', link: '/en/files' },
              { text: 'Web Terminal', link: '/en/web-terminal' },
              { text: 'Resource Management', link: '/en/resource-management' },
              { text: 'Platform Resources', link: '/en/platform-resources' },
              { text: 'Alerts', link: '/en/alerts' },
              { text: 'Dashboard', link: '/en/dashboard' },
              { text: 'Observability', link: '/en/observability' },
              { text: 'AI Bundle', link: '/en/ai' },
              { text: 'Backup & Restore', link: '/en/backups' },
              { text: 'Team Access', link: '/en/team-access' },
              { text: 'Project Members', link: '/en/project-members' },
              { text: 'Authentication', link: '/en/authentication' },
              { text: 'Security', link: '/en/security' },
              { text: 'Cluster Migration', link: '/en/migration' },
              { text: 'Configuration', link: '/en/configuration' },
              { text: 'GitOps', link: '/en/gitops' },
              { text: 'Blueprints', link: '/en/blueprints' },
            ],
          },
          {
            text: 'Reference',
            items: [
              { text: 'Architecture', link: '/en/architecture' },
              { text: 'Git Providers', link: '/en/git-providers' },
              { text: 'Kipper vs other self-hosted PaaS', link: '/en/comparison' },
              { text: 'FAQ', link: '/en/faq' },
            ],
          },
          {
            text: 'Contributing',
            items: [
              { text: 'Contributing Guide', link: '/en/contributing' },
            ],
          },
        ],
      },

      socialLinks: [
        { icon: 'github', link: 'https://github.com/getkipper/kipper' },
      ],

      footer: {
        message: 'Released under the Apache 2.0 License.',
        copyright: 'Copyright © 2026 Labb Consulting',
      },

      search: {
        provider: 'local',
      },
    },
  }),
)
