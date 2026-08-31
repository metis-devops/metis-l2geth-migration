package migration

func prefixLimit(prefix []byte) []byte {
	limit := append([]byte(nil), prefix...)
	for i := len(limit) - 1; i >= 0; i-- {
		if limit[i] != 0xff {
			limit[i]++
			return limit[:i+1]
		}
	}
	return nil
}

func prefixedKey(prefix, suffix []byte) []byte {
	key := make([]byte, 0, len(prefix)+len(suffix))
	key = append(key, prefix...)
	key = append(key, suffix...)
	return key
}
