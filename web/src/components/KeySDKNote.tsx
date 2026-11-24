// KeySDKNote: a key in the current format (wst_ and 50 characters, no app
// ID) needs an SDK that reads it (0.2.0); older ones refuse it as "an app
// key is wst_test_… or wst_live_…". The note names 0.3.0,
// the first release that tells Wakeshift which tool a result is from, which
// updated hooks need to connect wake-ups: a hook binds the beacon in a tool
// result to the tool that returned it.

// DEFAULT_CORE is production's core, where the SDKs send a current key
// unless WAKESHIFT_CORE says otherwise.
export const DEFAULT_CORE = 'https://app.wakeshift.ai'

export function KeySDKNote({ appKey }: { appKey: string }) {
  if (/^wst_(test|live)_/.test(appKey)) return null
  const core = window.location.origin
  return (
    <p className="mt-2 text-sm text-ink-3">
      Use <code>@wakeshift/sdk</code> 0.3.0 or later, <code>wakeshift</code> 0.3.0 or later for Python, or the Go SDK
      (<code>github.com/wakeshift/wakeshift-go</code>, whose client is <code>wakeshift.New</code>) v0.3.0 or later: earlier versions refuse this key, or don&apos;t connect every agent&apos;s wake-ups.
      {core !== DEFAULT_CORE && (
        <>
          {' '}
          It works against {core}: set <code>WAKESHIFT_CORE={core}</code> too, or the SDKs send it to {DEFAULT_CORE}.
        </>
      )}
    </p>
  )
}
