<script setup lang="ts">
import { RouterView, RouterLink, useRoute, useRouter } from 'vue-router'
import { LayoutDashboard, FolderKanban, Rocket, Database, Zap, Timer, Globe, HardDrive, FolderOpen, Archive, BarChart3, Users, LogOut, Sun, Moon, ExternalLink, Settings, ArrowRightLeft, ServerCog, Menu } from 'lucide-vue-next'
import AlertBell from '@/components/AlertBell.vue'
import TwoFactorNudge from '@/components/TwoFactorNudge.vue'
import { computed, nextTick, onMounted, onUnmounted, ref, watch, type Component } from 'vue'
import { useAuthStore } from '@/stores/auth'
import { useProjectsStore } from '@/stores/projects'
import { useDarkMode } from '@/composables/useDarkMode'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const projectsStore = useProjectsStore()
const { isDark, toggle } = useDarkMode()

// The sidebar is a slide-in drawer below the md breakpoint; closing it on
// navigation keeps it from covering the page after a tap.
const sidebarOpen = ref(false)
watch(() => route.path, () => { sidebarOpen.value = false })

const drawerRef = ref<HTMLElement | null>(null)
// The element focused before the drawer opened, so focus returns there on close.
let previouslyFocused: HTMLElement | null = null

function drawerFocusable(): HTMLElement[] {
  if (!drawerRef.value) return []
  const selector =
    'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])'
  return Array.from(drawerRef.value.querySelectorAll<HTMLElement>(selector))
}

// Keep Tab within the open drawer so focus can't wander to the page behind it.
function trapTab(e: KeyboardEvent) {
  const focusable = drawerFocusable()
  if (focusable.length === 0) {
    e.preventDefault()
    return
  }
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  const active = document.activeElement
  if (e.shiftKey && (active === first || active === drawerRef.value)) {
    e.preventDefault()
    last.focus()
  } else if (!e.shiftKey && active === last) {
    e.preventDefault()
    first.focus()
  }
}

// Escape closes the open drawer and Tab is trapped inside it, matching the
// modal dialogs. Both only apply while the drawer is open (mobile only).
function onKeydown(e: KeyboardEvent) {
  if (!sidebarOpen.value) return
  if (e.key === 'Escape') {
    sidebarOpen.value = false
  } else if (e.key === 'Tab') {
    trapTab(e)
  }
}

// Move focus into the drawer when it opens and restore it when it closes.
watch(sidebarOpen, (open) => {
  if (open) {
    previouslyFocused = document.activeElement as HTMLElement | null
    nextTick(() => {
      const focusable = drawerFocusable()
      ;(focusable[0] ?? drawerRef.value)?.focus()
    })
  } else if (previouslyFocused) {
    previouslyFocused.focus()
    previouslyFocused = null
  }
})

// Close the drawer once the viewport reaches the md breakpoint so its focus
// trap and dialog semantics never linger on the static desktop sidebar after a
// resize or rotation.
const desktopQuery = typeof window !== 'undefined' ? window.matchMedia('(min-width: 768px)') : null
function closeOnDesktop(e: MediaQueryListEvent) {
  if (e.matches) sidebarOpen.value = false
}

onMounted(() => {
  projectsStore.loadProjects()
  window.addEventListener('keydown', onKeydown)
  desktopQuery?.addEventListener('change', closeOnDesktop)
})
onUnmounted(() => {
  window.removeEventListener('keydown', onKeydown)
  desktopQuery?.removeEventListener('change', closeOnDesktop)
})

interface NavItem { name: string; to: string; icon: Component }
interface NavSection { label: string; items: NavItem[] }

const navSections = computed<NavSection[]>(() => {
  const sections: NavSection[] = [
    { label: '', items: [
      { name: 'Dashboard', to: '/', icon: LayoutDashboard },
      { name: 'Projects', to: '/projects', icon: FolderKanban },
    ] },
    { label: 'Workloads', items: [
      { name: 'Apps', to: '/apps', icon: Rocket },
      { name: 'Functions', to: '/functions', icon: Zap },
      { name: 'Jobs', to: '/jobs', icon: Timer },
    ] },
    { label: 'Networking', items: [
      { name: 'Routes', to: '/routes', icon: Globe },
    ] },
    { label: 'Data', items: [
      { name: 'Services', to: '/services', icon: Database },
      { name: 'Volumes', to: '/volumes', icon: HardDrive },
      { name: 'Object storage', to: '/storage', icon: FolderOpen },
      { name: 'Backups', to: '/backups', icon: Archive },
    ] },
  ]
  if (auth.isAdmin) {
    sections.push({ label: 'Administration', items: [
      { name: 'Users', to: '/users', icon: Users },
      { name: 'Platform', to: '/platform', icon: ServerCog },
      { name: 'Migration', to: '/migration', icon: ArrowRightLeft },
      { name: 'Settings', to: '/settings', icon: Settings },
    ] })
  }
  return sections
})

// The mobile bottom bar carries the most-visited destinations; everything else
// stays reachable through the drawer via its Menu button.
const bottomNav: NavItem[] = [
  { name: 'Dashboard', to: '/', icon: LayoutDashboard },
  { name: 'Apps', to: '/apps', icon: Rocket },
  { name: 'Services', to: '/services', icon: Database },
  { name: 'Functions', to: '/functions', icon: Zap },
]

const grafanaUrl = (() => {
  const host = window.location.hostname
  // Match the SubdomainFor convention: kipper.run uses a double-dash
  // separator (`console--foo.kipper.run` → `grafana--foo.kipper.run`),
  // custom domains use a dot (`console.example.com` → `grafana.example.com`).
  return `https://${host.replace(/^console(--|\.)/, 'grafana$1')}`
})()

function isActive(path: string): boolean {
  if (path === '/') return route.path === '/'
  return route.path.startsWith(path)
}

function handleLogout() {
  auth.logout()
  router.push('/login')
}
</script>

<template>
  <div class="flex h-screen overflow-hidden bg-slate-50 dark:bg-slate-950">
    <!-- Drawer backdrop (mobile only) -->
    <div
      v-if="sidebarOpen"
      class="fixed inset-0 z-40 bg-black/40 md:hidden"
      @click="sidebarOpen = false"
    />

    <!-- Sidebar: a slide-in drawer on mobile, static from md up -->
    <aside
      ref="drawerRef"
      tabindex="-1"
      class="fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r border-slate-200 bg-white transition-transform duration-200 focus:outline-none dark:border-slate-800 dark:bg-slate-900 md:static md:z-auto md:translate-x-0"
      :class="sidebarOpen ? 'translate-x-0' : '-translate-x-full'"
      :role="sidebarOpen ? 'dialog' : undefined"
      :aria-modal="sidebarOpen ? 'true' : undefined"
      aria-label="Sidebar"
    >
      <!-- Logo -->
      <div class="flex shrink-0 items-center border-b border-slate-200 px-6 py-4 dark:border-slate-800">
        <img src="/logo-horizontal-light.svg" alt="Kipper" class="h-8 object-contain dark:hidden" />
        <img src="/logo-horizontal-dark.svg" alt="Kipper" class="hidden h-8 object-contain dark:block" />
      </div>

      <!-- Global project selector -->
      <div class="shrink-0 border-b border-slate-200 px-3 py-2 dark:border-slate-800">
        <select
          :value="projectsStore.globalNamespace"
          @change="projectsStore.setGlobalNamespace(($event.target as HTMLSelectElement).value)"
          class="w-full rounded-md border border-slate-200 bg-slate-50 px-2.5 py-1.5 text-xs font-medium text-slate-700 dark:border-slate-700 dark:bg-slate-800 dark:text-slate-300"
        >
          <option v-for="opt in projectsStore.namespaceOptions" :key="opt.value" :value="opt.value">{{ opt.label }}</option>
        </select>
      </div>

      <!-- Nav links. A tap anywhere here closes the drawer, including a tap on
           the current route (where the route watcher wouldn't fire). -->
      <nav class="min-h-0 flex-1 space-y-3 overflow-y-auto px-3 py-4" @click="sidebarOpen = false">
        <div v-for="section in navSections" :key="section.label || 'main'" class="space-y-1">
          <p
            v-if="section.label"
            class="px-3 pb-1 pt-1 text-[10px] font-semibold uppercase tracking-wider text-slate-400 dark:text-slate-500"
          >
            {{ section.label }}
          </p>
          <RouterLink
            v-for="item in section.items"
            :key="item.name"
            :to="item.to"
            class="flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors"
            :class="isActive(item.to)
              ? 'bg-kipper-50 text-kipper-700 dark:bg-kipper-950 dark:text-kipper-300'
              : 'text-slate-600 hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200'"
          >
            <component :is="item.icon" class="h-5 w-5" :stroke-width="1.75" />
            {{ item.name }}
          </RouterLink>
        </div>
        <!-- Grafana external link -->
        <a
          :href="grafanaUrl"
          target="_blank"
          rel="noopener"
          class="flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        >
          <BarChart3 class="h-5 w-5" :stroke-width="1.75" />
          Grafana
          <ExternalLink class="ml-auto h-3.5 w-3.5 opacity-50" />
        </a>
      </nav>

      <!-- Bottom controls -->
      <div class="shrink-0 border-t border-slate-200 p-3 dark:border-slate-800">
        <AlertBell />
        <button
          @click="toggle"
          class="flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-slate-100 hover:text-slate-900 dark:text-slate-400 dark:hover:bg-slate-800 dark:hover:text-slate-200"
        >
          <Moon v-if="!isDark" class="h-5 w-5" :stroke-width="1.75" />
          <Sun v-else class="h-5 w-5" :stroke-width="1.75" />
          {{ isDark ? 'Light mode' : 'Dark mode' }}
        </button>
        <button
          @click="handleLogout"
          class="flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium text-slate-600 transition-colors hover:bg-red-50 hover:text-red-700 dark:text-slate-400 dark:hover:bg-red-950 dark:hover:text-red-400"
        >
          <LogOut class="h-5 w-5" :stroke-width="1.75" />
          Sign out
        </button>
      </div>
    </aside>

    <!-- Right column: mobile top bar + main content -->
    <div class="flex flex-1 flex-col overflow-hidden">
      <!-- Mobile top bar with the drawer toggle (hidden from md up) -->
      <div class="flex items-center gap-3 border-b border-slate-200 bg-white px-4 py-3 dark:border-slate-800 dark:bg-slate-900 md:hidden">
        <button
          class="rounded-lg p-1.5 text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800"
          aria-label="Open menu"
          @click="sidebarOpen = true"
        >
          <Menu class="h-6 w-6" :stroke-width="1.75" />
        </button>
        <img src="/logo-horizontal-light.svg" alt="Kipper" class="h-7 object-contain dark:hidden" />
        <img src="/logo-horizontal-dark.svg" alt="Kipper" class="hidden h-7 object-contain dark:block" />
      </div>

      <!-- Main content. Routes can opt out of the centred-with-padding
           wrapper by setting `meta.fullBleed: true` — useful for full-
           viewport surfaces like the database console where the editor,
           schema sidebar, and AI panel need every pixel of horizontal
           space. -->
      <main class="flex flex-1 flex-col overflow-hidden">
        <TwoFactorNudge />
        <template v-if="$route.meta?.fullBleed">
          <RouterView class="flex-1 min-h-0 overflow-hidden" />
        </template>
        <template v-else>
          <div class="flex-1 overflow-y-auto">
            <div class="mx-auto max-w-6xl px-4 py-6 md:px-8 md:py-8">
              <RouterView />
            </div>
          </div>
        </template>
      </main>

      <!-- Mobile bottom nav (hidden from md up) -->
      <nav
        aria-label="Primary"
        class="flex border-t border-slate-200 bg-white pb-[env(safe-area-inset-bottom)] dark:border-slate-800 dark:bg-slate-900 md:hidden"
      >
        <RouterLink
          v-for="item in bottomNav"
          :key="item.name"
          :to="item.to"
          class="flex flex-1 flex-col items-center gap-1 py-2 text-[11px] font-medium transition-colors"
          :class="isActive(item.to)
            ? 'text-kipper-600 dark:text-kipper-300'
            : 'text-slate-600 dark:text-slate-400'"
        >
          <component :is="item.icon" class="h-5 w-5" :stroke-width="1.75" />
          {{ item.name }}
        </RouterLink>
        <button
          class="flex flex-1 flex-col items-center gap-1 py-2 text-[11px] font-medium text-slate-600 dark:text-slate-400"
          @click="sidebarOpen = true"
        >
          <Menu class="h-5 w-5" :stroke-width="1.75" />
          Menu
        </button>
      </nav>
    </div>
  </div>
</template>
