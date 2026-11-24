// The development sign-in token (WAKESHIFT_DEV_SIGN_IN on a local server):
// the server prints it at startup with a link, /?dev_token=<token>. The
// sign-in page takes the link's token out of the address bar and keeps it
// for this tab (sessionStorage), so it and the join page can fill it in.

const storageKey = 'wakeshift.dev-token'

// devTokenParam is the link's query parameter.
export const devTokenParam = 'dev_token'

// storedDevToken is the token this tab kept, if any.
export function storedDevToken(): string {
  try {
    return sessionStorage.getItem(storageKey) ?? ''
  } catch {
    return ''
  }
}

// keepDevToken keeps a token for this tab (one entered by hand, too).
export function keepDevToken(token: string) {
  try {
    sessionStorage.setItem(storageKey, token)
  } catch {
    // Private mode: it lasts this page only.
  }
}
