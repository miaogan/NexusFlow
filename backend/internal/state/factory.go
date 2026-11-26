package state

// NewStore selects the task store implementation from configuration.
func NewStore(driver, redisAddr string) (Store, error) {
	switch driver {
	case "redis":
		return NewRedisStore(redisAddr)
	default:
		return NewMachine(), nil
	}
}
