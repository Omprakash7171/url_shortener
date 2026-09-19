package model

import "time"

type DailyCount struct {
	Day    time.Time
	Clicks int64
}

type ReferrerCount struct {
	Referrer string
	Clicks   int64
}

type StatsSummary struct {
	TotalClicks  int64
	Daily        []DailyCount
	TopReferrers []ReferrerCount
}