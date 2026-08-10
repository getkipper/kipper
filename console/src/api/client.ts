import axios, { AxiosError } from 'axios'

// friendlyMessage turns an axios error into something a person can act on,
// preferring the server's own message and otherwise mapping the status code.
// Without this, callers surface raw text like "Request failed with status
// code 500" straight to the user.
function friendlyMessage(error: AxiosError): string {
  const data = error.response?.data as { error?: string; message?: string } | undefined
  const serverMsg = data?.error || data?.message
  if (serverMsg) return serverMsg

  if (!error.response) {
    return 'Cannot reach the server. Check your connection and try again.'
  }

  switch (error.response.status) {
    case 400:
      return 'That request was not valid. Check the details and try again.'
    case 401:
      return 'Your session has expired. Please sign in again.'
    case 403:
      return 'You do not have permission to do that.'
    case 404:
      return 'That could not be found. It may have been deleted.'
    case 409:
      return 'That conflicts with the current state. Refresh and try again.'
    case 429:
      return 'Too many requests. Wait a moment and try again.'
    case 500:
    case 502:
    case 503:
    case 504:
      return 'The server ran into a problem. Please try again in a moment.'
    default:
      return `Something went wrong (status ${error.response.status}).`
  }
}

const client = axios.create({
  baseURL: import.meta.env.VITE_API_URL || '/api/v1',
  headers: {
    'Content-Type': 'application/json',
  },
})

client.interceptors.request.use((config) => {
  const token = localStorage.getItem('kipper_token')
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

client.interceptors.response.use(
  (response) => response,
  (error: AxiosError) => {
    if (error.response?.status === 401 && window.location.pathname !== '/login') {
      localStorage.removeItem('kipper_token')
      window.location.href = '/login'
    }
    // Replace the raw axios text so callers that show error.message present
    // something readable.
    error.message = friendlyMessage(error)
    return Promise.reject(error)
  },
)

export default client
