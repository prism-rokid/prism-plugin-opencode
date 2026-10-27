package adapter

func terminalReason(eventType string, props map[string]any) string {
	switch eventType {
	case "session.execution.failed":
		return "failed"
	case "session.execution.interrupted":
		return "interrupted"
	default:
		return idleReason(props)
	}
}
