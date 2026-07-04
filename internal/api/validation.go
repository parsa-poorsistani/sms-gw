package api

import "regexp"

var phoneRegexp = regexp.MustCompile(`^\+?[0-9]{7,15}$`)

func validPhone(phone string) bool {
	return phoneRegexp.MatchString(phone)
}
