package main

import (
	"time"
	_ "time/tzdata"
)

type DateRange struct {
	StartDate, EndDate string
	Zone               string
	StartUTC, EndUTC   time.Time
}

func calendarRange(start, end string, zones ...string) (DateRange, error) {
	name := "America/Los_Angeles"
	if len(zones) > 0 {
		name = zones[0]
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return DateRange{}, failure("timezone_unavailable")
	}
	first, err := time.ParseInLocation(time.DateOnly, start, zone)
	if err != nil {
		return DateRange{}, failure("invalid_date_range")
	}
	last, err := time.ParseInLocation(time.DateOnly, end, zone)
	if err != nil || !validCalendarRange(first, last, start, end) {
		return DateRange{}, failure("invalid_date_range")
	}
	return DateRange{StartDate: start, EndDate: end, StartUTC: first.UTC(), EndUTC: last.AddDate(0, 0, 1).UTC(), Zone: name}, nil
}
func parseDateRange(start, end string, now time.Time, zones ...string) (DateRange, error) {
	result, err := calendarRange(start, end, zones...)
	if err != nil {
		return result, err
	}
	zone, _ := time.LoadLocation(result.Zone)
	if end >= now.In(zone).Format(time.DateOnly) {
		return DateRange{}, failure("invalid_date_range")
	}
	return result, nil
}
func nextChunk(request DateRange, nextDate string) (DateRange, error) {
	if nextDate < request.StartDate || nextDate > request.EndDate {
		return DateRange{}, failure("invalid_cursor")
	}
	zone, _ := time.LoadLocation(request.Zone)
	first, err := time.ParseInLocation(time.DateOnly, nextDate, zone)
	if err != nil {
		return DateRange{}, failure("invalid_cursor")
	}
	last := first.AddDate(0, 0, 6).Format(time.DateOnly)
	last = min(last, request.EndDate)
	return calendarRange(nextDate, last, request.Zone)
}

// Count UTC hourly samples in the local-day interval, including fractional offsets.
func hourlyCount(chunk DateRange) int {
	first := chunk.StartUTC.Truncate(time.Hour)
	if first.Before(chunk.StartUTC) {
		first = first.Add(time.Hour)
	}
	if !first.Before(chunk.EndUTC) {
		return 0
	}
	return int((chunk.EndUTC.Sub(first) + time.Hour - 1) / time.Hour)
}

func validCalendarRange(first, last time.Time, start, end string) bool {
	return first.Format(time.DateOnly) == start && last.Format(time.DateOnly) == end && start >= "1940-01-01" && !last.Before(first) && !last.After(first.AddDate(0, 0, 365))
}
