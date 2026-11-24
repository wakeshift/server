package notify

// Connects is how many listener connections h has made.
func Connects(h *Hub) int64 { return h.connects.Load() }
