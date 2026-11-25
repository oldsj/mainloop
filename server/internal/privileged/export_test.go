package privileged

// Refusal is a refusal as the executor's own clients return one, for fakes of them.
func Refusal(msg string) error { return refuse("%s", msg) }
