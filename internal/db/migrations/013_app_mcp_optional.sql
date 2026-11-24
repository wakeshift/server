-- Apps that only publish into workspaces (Sources) have no MCP server.
ALTER TABLE apps ALTER COLUMN mcp_url DROP NOT NULL;
