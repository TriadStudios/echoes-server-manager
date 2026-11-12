package doctor

// writable is not checked on Windows (it depends on ACLs).
func writable(string) (ok, known bool) { return true, false }
