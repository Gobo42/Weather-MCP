package main

import (
	"testing"
	"time"
)

func TestDateRange(testingContext *testing.T) {
	now := time.Date(2027, 1, 10, 12, 0, 0, 0, time.UTC)
	for _, example := range []struct {
		start, end string
		hours      int
	}{{"2026-03-08", "2026-03-08", 23}, {"2026-11-01", "2026-11-01", 25}, {"2024-02-29", "2024-02-29", 24}, {"1940-01-01", "1940-01-01", 24}, {"2024-01-01", "2024-12-31", 8784}} {
		result, err := parseDateRange(example.start, example.end, now)
		if err != nil || result.EndUTC.Sub(result.StartUTC) != time.Duration(example.hours)*time.Hour {
			testingContext.Fatalf("%+v result=%+v err=%v", example, result, err)
		}
	}
	for _, example := range [][2]string{{"1939-12-31", "1940-01-01"}, {"2026-02-29", "2026-03-01"}, {"2026-02-02", "2026-02-01"}, {"2024-01-01", "2025-01-01"}, {"2027-01-10", "2027-01-10"}, {"2026-1-1", "2026-01-02"}} {
		if _, err := parseDateRange(example[0], example[1], now); err == nil {
			testingContext.Fatalf("accepted %v", example)
		}
	}
}
func TestChunk(testingContext *testing.T) {
	request, err := parseDateRange("2026-03-01", "2026-03-20", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		testingContext.Fatal(err)
	}
	chunk, err := nextChunk(request, "2026-03-08")
	if err != nil || chunk.EndDate != "2026-03-14" || chunk.EndUTC.Sub(chunk.StartUTC) != 167*time.Hour {
		testingContext.Fatalf("%+v %v", chunk, err)
	}
	chunk, err = nextChunk(request, "2026-03-19")
	if err != nil || chunk.EndDate != "2026-03-20" {
		testingContext.Fatal(chunk, err)
	}
	if _, err = nextChunk(request, "2026-02-28"); err == nil {
		testingContext.Fatal("accepted cursor outside request")
	}
}
