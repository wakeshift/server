import { Logo, useTitle } from '../components/ui'
import './authorize.css'

// The OAuth authorize page, where an MCP client once asked to connect one of
// the user's agents to an app with their Wakeshift account. Apps on
// Wakeshift keep their own users now, so it only says so: the app signs
// people in itself.
export default function Authorize() {
  useTitle('Connect an agent')
  return (
    <div className="az">
      <div className="az-brand">
        <Logo />
      </div>
      <div className="az-card">
        <h1>Can't connect</h1>
        <p className="az-error">Apps no longer sign you in with your Wakeshift account.</p>
        <p className="az-note">
          Each app signs you in itself. Go back to your agent and connect to the app again; if it still sends you here,
          tell the app's developer. <a href="/">Open the console</a>
        </p>
      </div>
    </div>
  )
}
