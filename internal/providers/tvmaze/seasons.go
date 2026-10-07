package tvmaze

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxKnownSeasons = 100

// SeasonSummary intentionally exposes only the count of season records TVMaze
// currently returns. It does not treat episodeOrder as an aired episode count.
type SeasonSummary struct {
	SeasonCount int
}

// GetSeasonSummary is a Details-only lookup for one exact TVMaze show ID. It
// makes one /shows/:id/seasons request and never requests episode lists.
func (source *SearchSource) GetSeasonSummary(ctx context.Context, externalID string) (SeasonSummary, error) {
	if !validShowID(externalID) {
		return SeasonSummary{}, ErrInvalidExternalID
	}
	requestContext, cancel := context.WithTimeout(ctx, source.requestTimeout)
	defer cancel()
	if err := source.limiter.Wait(requestContext); err != nil {
		return SeasonSummary{}, err
	}
	showURL, err := detailsURL(source.endpoint, externalID)
	if err != nil {
		return SeasonSummary{}, ErrDetailsUnavailable
	}
	seasonURL, err := url.Parse(showURL)
	if err != nil {
		return SeasonSummary{}, ErrDetailsUnavailable
	}
	seasonURL.Path = strings.TrimRight(seasonURL.Path, "/") + "/seasons"
	seasonURL.RawPath = ""
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, seasonURL.String(), nil)
	if err != nil {
		return SeasonSummary{}, ErrDetailsUnavailable
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	response, err := source.httpClient.Do(request)
	if err != nil {
		if requestContext.Err() != nil {
			return SeasonSummary{}, requestContext.Err()
		}
		return SeasonSummary{}, ErrDetailsUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return SeasonSummary{}, ErrWorkNotFound
	case http.StatusTooManyRequests:
		return SeasonSummary{}, ErrDetailsRateLimited
	default:
		return SeasonSummary{}, ErrDetailsUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBodyBytes+1))
	if err != nil {
		if requestContext.Err() != nil {
			return SeasonSummary{}, requestContext.Err()
		}
		return SeasonSummary{}, ErrDetailsUnavailable
	}
	if len(body) > MaxResponseBodyBytes {
		return SeasonSummary{}, ErrDetailsResponseTooLarge
	}
	var seasons []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(body, &seasons); err != nil {
		return SeasonSummary{}, fmt.Errorf("%w: invalid seasons array", ErrDetailsMalformedResponse)
	}
	if seasons == nil || len(seasons) > maxKnownSeasons {
		return SeasonSummary{}, ErrDetailsMalformedResponse
	}
	for _, season := range seasons {
		if season.ID < 1 {
			return SeasonSummary{}, ErrDetailsMalformedResponse
		}
	}
	return SeasonSummary{SeasonCount: len(seasons)}, nil
}
