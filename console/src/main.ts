import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import './assets/fonts.css'
import './assets/main.css'
import 'highlight.js/styles/github-dark.css'

const app = createApp(App)

app.use(createPinia())
app.use(router)
app.mount('#app')
