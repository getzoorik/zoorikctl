package text

import "strings"

func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func OrText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func OrNone(value string) string {
	return OrText(value, "none")
}
