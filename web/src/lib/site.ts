import { useQuery } from '@tanstack/react-query'
import { getSession, type Site } from '../api/client'

// Before the session loads: Wakeshift's docs, no Terms (the server's
// auth.PublicDocs and apps.DefaultSupportEmail).
const fallback: Site = { docs: 'https://wakeshift.ai/docs', support_email: 'support@wakeshift.ai' }

// useSite is what the console links to and names, as the server is
// configured (GET /api/v1/session's site: WAKESHIFT_SITE_URL,
// WAKESHIFT_TERMS_URL, WAKESHIFT_PRIVACY_URL, WAKESHIFT_SUPPORT_EMAIL).
export function useSite(): Site {
  const session = useQuery({ queryKey: ['session'], queryFn: getSession, staleTime: Infinity })
  return session.data?.site ?? fallback
}

// wakeshiftTerms reports whether the Terms are Wakeshift's own (served from
// wakeshift.ai, as on its hosted service), which the sign-in and join pages
// name as Wakeshift's; anyone else's are "the Terms of Service".
export function wakeshiftTerms(site: Site): boolean {
  if (!site.terms) return false
  try {
    const host = new URL(site.terms).hostname
    return host === 'wakeshift.ai' || host.endsWith('.wakeshift.ai')
  } catch {
    return false
  }
}
