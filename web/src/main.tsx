import React from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import App from './App'
import { ApiError } from './api/client'
import './index.css'

// A 401 anywhere means the console session ended: re-check it, which shows
// the sign-in page.
const onError = (error: Error) => {
  if (error instanceof ApiError && error.status === 401) {
    queryClient.invalidateQueries({ queryKey: ['session'] })
  }
}

const queryClient: QueryClient = new QueryClient({
  queryCache: new QueryCache({ onError }),
  mutationCache: new MutationCache({ onError }),
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      retry: (count, error) => !(error instanceof ApiError && error.status < 500) && count < 1,
    },
  },
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </BrowserRouter>
  </React.StrictMode>,
)
