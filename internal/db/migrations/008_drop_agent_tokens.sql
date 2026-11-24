-- Agents don't hold Wakeshift credentials: they sign in to apps (Sign in
-- with Wakeshift), and apps call the core. The console's static agent
-- tokens existed only for a core MCP endpoint that's gone.
DROP TABLE agent_tokens;
