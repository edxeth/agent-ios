package commands

// close ends only the caller's session (design 4.2 row "close"). It has no
// --all: that flag is rejected as unknown (exit 2) before any connection,
// so one agent can never end another agent's session.
func init() {
	Register(&Command{
		Name: "close", MCPName: "close_session", Profile: ProfileCore,
		Summary: "End this session (--session): release its page leases and run their cleanup. The Safari tab stays open; other sessions are not touched.",
		Run:     closeSession,
	})
}
