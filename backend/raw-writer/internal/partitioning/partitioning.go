// Package partitioning derives the event_date/hour partition of an event and builds
// object keys of the form <prefix>/app_id=<app>/event_date=YYYY-MM-DD/hour=HH/<file>.
package partitioning

import (
	"strconv"
	"strings"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
)

// ParseISODatetime mirrors the original parse_iso_datetime: falsy values and strings
// fromisoformat rejects give ok=false; a trailing "Z" means UTC; naive values are UTC.
// It fails with AttributeError for truthy non-strings and OverflowError when the UTC
// conversion leaves the year 1..9999 range.
func ParseISODatetime(value any) (time.Time, bool, error) {
	if !pyval.Truthy(value) {
		return time.Time{}, false, nil
	}
	s, isStr := value.(string)
	if !isStr {
		return time.Time{}, false, pyval.ErrAttribute
	}
	candidate := pyval.Strip(s)
	if strings.HasSuffix(candidate, "Z") {
		candidate = candidate[:len(candidate)-1] + "+00:00"
	}
	res, ok := fromISOFormat(candidate)
	if !ok {
		return time.Time{}, false, nil
	}
	t := time.Date(res.year, time.Month(res.month), res.day, res.hour, res.minute, res.second, res.micros*1000, time.UTC)
	if res.hasTZ && res.offsetMicros != 0 {
		t = t.Add(-time.Duration(res.offsetMicros) * time.Microsecond)
		if t.Year() < 1 || t.Year() > 9999 {
			return time.Time{}, false, pyval.ErrOverflow
		}
	}
	return t, true, nil
}

// EventPartition returns (event_date, hour) from event_timestamp, else the fallback
// timestamp, else now.
func EventPartition(eventTimestamp, fallbackTimestamp any, now func() time.Time) (string, string, error) {
	t, ok, err := ParseISODatetime(eventTimestamp)
	if err != nil {
		return "", "", err
	}
	if !ok {
		if t, ok, err = ParseISODatetime(fallbackTimestamp); err != nil {
			return "", "", err
		}
	}
	if !ok {
		t = now().UTC()
	}
	return FormatDate(t), t.Format("15"), nil
}

// FormatDate is strftime("%Y-%m-%d") on glibc, which does not zero-pad years below 1000.
func FormatDate(t time.Time) string {
	return strconv.Itoa(t.Year()) + t.Format("-01-02")
}

// ObjectPartitionPath joins the trimmed prefix with the partition directories and file.
func ObjectPartitionPath(prefix, appID, eventDate, hour, filename string) string {
	suffix := "app_id=" + appID + "/event_date=" + eventDate + "/hour=" + hour + "/" + filename
	cleaned := strings.Trim(prefix, "/")
	if cleaned == "" {
		return suffix
	}
	return cleaned + "/" + suffix
}
