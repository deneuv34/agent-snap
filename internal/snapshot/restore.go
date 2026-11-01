package snapshot

func ValidObjectID(id string) bool {
	return (len(id) == 40 || len(id) == 64) && isLowerHex(id)
}
