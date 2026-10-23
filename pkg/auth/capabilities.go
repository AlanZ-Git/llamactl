package auth

// N1 capabilities are opt-in rows, not implied by Controller or allow_all.
func ValidCapability(capability string) bool {
	switch capability {
	case "discover", "evaluate", "recipe_write", "recipe_publish", "service_use", "inference", "observe":
		return true
	}
	return false
}
