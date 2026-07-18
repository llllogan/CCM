package util

import "time"

var brisbaneLocation = loadBrisbaneLocation()

func loadBrisbaneLocation() *time.Location {
	location, err := time.LoadLocation("Australia/Brisbane")
	if err == nil {
		return location
	}
	return time.FixedZone("AEST", 10*60*60)
}

// BrisbaneTime converts an instant to the timezone used in CCM notifications.
func BrisbaneTime(value time.Time) time.Time {
	return value.In(brisbaneLocation)
}
