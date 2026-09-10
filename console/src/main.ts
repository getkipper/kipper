import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import { getAppearance } from './api/appearance'
import { applyFaviconColour } from './utils/favicon'
import './assets/fonts.css'
import './assets/main.css'
import 'highlight.js/styles/github-dark.css'

const app = createApp(App)

app.use(createPinia())
app.use(router)
app.mount('#app')

// The tab icon carries this cluster's colour, so somebody with several consoles
// open can tell the tabs apart.
getAppearance()
  .then((settings) => applyFaviconColour(settings.faviconColour))
  .catch(() => undefined)
