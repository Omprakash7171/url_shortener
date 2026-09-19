package repository

import (
	"context"
	"fmt"

	"urlshortener/internal/model"
)

func (r *URLRepository) StatsByURLID(ctx context.Context, urlID int64) (*model.StatsSummary, error) {
	stats := &model.StatsSummary{
		Daily:        []model.DailyCount{},
		TopReferrers: []model.ReferrerCount{},
	}

	if err := r.pool.QueryRow(ctx,
		`SELECT coalesce(sum(count), 0) FROM analytics_daily WHERE url_id = $1 AND dim = 'total'`,
		urlID,
	).Scan(&stats.TotalClicks); err != nil {
		return nil, fmt.Errorf("sum total clicks: %w", err)
	}

	rows, err := r.pool.Query(ctx,
		`SELECT day, count FROM analytics_daily WHERE url_id = $1 AND dim = 'total' ORDER BY day`,
		urlID,
	)
	if err != nil {
		return nil, fmt.Errorf("query daily clicks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d model.DailyCount
		if err := rows.Scan(&d.Day, &d.Clicks); err != nil {
			return nil, fmt.Errorf("scan daily click: %w", err)
		}
		stats.Daily = append(stats.Daily, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate daily clicks: %w", err)
	}

	rows, err = r.pool.Query(ctx,
		`SELECT value, sum(count) AS total FROM analytics_daily
		 WHERE url_id = $1 AND dim = 'referrer'
		 GROUP BY value ORDER BY total DESC LIMIT 10`,
		urlID,
	)
	if err != nil {
		return nil, fmt.Errorf("query referrers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ref model.ReferrerCount
		if err := rows.Scan(&ref.Referrer, &ref.Clicks); err != nil {
			return nil, fmt.Errorf("scan referrer: %w", err)
		}
		stats.TopReferrers = append(stats.TopReferrers, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate referrers: %w", err)
	}

	return stats, nil
}