import { Navigate, Route, Routes, useLocation, useParams } from 'react-router-dom'
import Layout, { appPath, background, orgPath } from './components/Layout'
import { Button } from './components/ui'
import { useSession } from './hooks/useSession'
import Agents from './pages/Agents'
import Apps from './pages/Apps'
import Audit from './pages/Audit'
import Authorize from './pages/Authorize'
import Events from './pages/Events'
import Home from './pages/Home'
import Login from './pages/Login'
import Join from './pages/Join'
import NewApp from './pages/NewApp'
import NewOrg from './pages/NewOrg'
import OrgModal from './pages/org/OrgModal'
import NotFound from './pages/NotFound'
import WebhookSetup from './pages/WebhookSetup'
import Subscriptions from './pages/Subscriptions'
import AppActivity from './pages/app/Activity'
import AppKeys from './pages/app/Keys'
import AppLog from './pages/app/Log'
import AppOverview from './pages/app/Overview'
import AppPublishURLs from './pages/app/PublishURLs'
import AppSettings from './pages/app/Settings'
import AppSubscriptions from './pages/app/Subscriptions'
import AppUsers from './pages/app/Users'

// Old links: Build was the list of apps, and /build/<id> an app's log.
function OldAppLink() {
  const { id = '' } = useParams()
  return <Navigate replace to={appPath(id, 'log')} />
}

function OrgIndex() {
  const { org = '' } = useParams()
  return <Navigate replace to={orgPath(org)} state={useLocation().state} />
}

function AppIndex() {
  const { id = '' } = useParams()
  return <Navigate replace to={appPath(id)} />
}

export default function App() {
  const { session } = useSession()
  const location = useLocation()
  // A Grok Bot's webhook setup is public and stands alone: no sign-in.
  if (location.pathname === '/webhook-setup') return <WebhookSetup />
  if (session.isPending) {
    return <p className="p-8 text-sm text-ink-3">Loading…</p>
  }
  if (session.isError) {
    return (
      <div className="flex min-h-screen flex-col items-center justify-center gap-3">
        <p className="text-sm text-danger">Can't reach the server: {session.error.message}</p>
        <Button variant="secondary" onClick={() => session.refetch()}>
          Try again
        </Button>
      </div>
    )
  }
  // Joining an organization stands alone, signed in or not.
  if (location.pathname === '/join') return <Join />
  if (!session.data.authenticated) return <Login />
  // The OAuth authorize page stands alone, outside the console.
  if (location.pathname === '/oauth/authorize') return <Authorize />
  return (
    <Layout>
      {/* Keyed by workspace, so switching resets every page's state. An
          organization's pages are a modal (OrgModal) over the page they were
          opened from, or over Quick start when their URL is opened directly. */}
      <Routes key={session.data.workspace?.id} location={background(location) ?? location}>
        <Route path="/" element={<Home />} />
        <Route path="/new-app" element={<NewApp />} />
        <Route path="/new-org" element={<NewOrg />} />
        <Route path="/orgs/:org/*" element={<Home />} />
        <Route path="/apps/:id" element={<AppIndex />} />
        <Route path="/apps/:id/overview" element={<AppOverview />} />
        <Route path="/apps/:id/keys" element={<AppKeys />} />
        <Route path="/apps/:id/subscriptions" element={<AppSubscriptions />} />
        <Route path="/apps/:id/activity" element={<AppActivity />} />
        <Route path="/apps/:id/users" element={<AppUsers />} />
        <Route path="/apps/:id/publish-urls" element={<AppPublishURLs />} />
        <Route path="/apps/:id/log" element={<AppLog />} />
        <Route path="/apps/:id/settings" element={<AppSettings />} />
        <Route path="/build" element={<Navigate replace to="/" />} />
        <Route path="/build/:id" element={<OldAppLink />} />
        {/* Your own Wakeshift workspace, in the sidebar below the apps. */}
        <Route path="/agents" element={<Agents />} />
        <Route path="/apps" element={<Apps />} />
        <Route path="/subscriptions" element={<Subscriptions />} />
        <Route path="/events" element={<Events />} />
        <Route path="/audit" element={<Audit />} />
        <Route path="*" element={<NotFound />} />
      </Routes>
      <Routes>
        <Route path="/orgs/:org" element={<OrgIndex />} />
        <Route path="/orgs/:org/:tab" element={<OrgModal key={session.data.workspace?.id} />} />
        <Route path="*" element={null} />
      </Routes>
    </Layout>
  )
}
