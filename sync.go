package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"log"
	"reflect"
	"regexp"
	"strings"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
)

var (
	markerID  = regexp.MustCompile(`(?m)\bconnpass_event_id:\s*([0-9]+)\b`)
	markerKey = regexp.MustCompile(`\bconnpass_sync_key:\s*connpass:([0-9]+)\b`)
	eventURL  = regexp.MustCompile(`https?://(?:[a-zA-Z0-9-]+\.)?connpass\.com/event/([0-9]+)(?:/|\b)`)
	htmlTags  = regexp.MustCompile(`<[^>]*>`)
)

func private(e *calendar.Event, key string) string {
	if e.ExtendedProperties == nil {
		return ""
	}
	return e.ExtendedProperties.Private[key]
}

// Explicit restoration markers take precedence over links to related events.
// A bare link is used only when the description identifies one unique event.
func connpassID(e *calendar.Event) string {
	if id := private(e, "connpass_event_id"); id != "" {
		return id
	}
	text := strings.TrimSpace(html.UnescapeString(htmlTags.ReplaceAllString(e.Description, "\n")))
	for _, re := range []*regexp.Regexp{markerID, markerKey} {
		if m := re.FindStringSubmatch(text); len(m) > 1 {
			return m[1]
		}
	}
	if e.Source != nil {
		if m := eventURL.FindStringSubmatch(e.Source.Url); len(m) > 1 {
			return m[1]
		}
	}
	if m := eventURL.FindStringSubmatch(strings.TrimSpace(text[strings.LastIndex(text, "\n")+1:])); len(m) > 1 {
		lastLine := strings.TrimSpace(text[strings.LastIndex(text, "\n")+1:])
		if strings.Trim(lastLine, " <>") == m[0] {
			return m[1]
		}
	}
	matches := eventURL.FindAllStringSubmatch(html.UnescapeString(e.Description), -1)
	id := ""
	for _, m := range matches {
		if id != "" && id != m[1] {
			return ""
		}
		id = m[1]
	}
	return id
}

func listEvents(ctx context.Context, svc *calendar.Service, cal string, since time.Time) ([]*calendar.Event, error) {
	call := svc.Events.List(cal).ShowDeleted(false).MaxResults(2500)
	if !since.IsZero() {
		call = call.UpdatedMin(since.Format(time.RFC3339))
	}
	var all []*calendar.Event
	err := call.Pages(ctx, func(page *calendar.Events) error { all = append(all, page.Items...); return nil })
	return all, err
}

type syncStats struct{ Created, Updated, Unchanged int }
type change struct {
	sourceID, targetID string
	event              *calendar.Event
}

func sameTime(a, b *calendar.EventDateTime) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Date != "" || b.Date != "" {
		return a.Date == b.Date
	}
	at, ae := time.Parse(time.RFC3339, a.DateTime)
	bt, be := time.Parse(time.RFC3339, b.DateTime)
	return ae == nil && be == nil && at.Equal(bt)
}

func description(src, existing *calendar.Event, id string) string {
	// Historical restores are archival records: retain their full text/excerpt
	// and batch marker instead of replacing it with a shrinking feed description.
	if existing != nil && strings.Contains(existing.Description, "connpass_import_batch:") {
		return existing.Description
	}
	d := src.Description
	if id != "" && !markerID.MatchString(d) {
		d += "\n\n---\nconnpass_event_id: " + id + "\nconnpass_sync_key: connpass:" + id
	}
	return d
}

func planEvents(sources, destinations []*calendar.Event, sourceCal, destCal string) ([]change, syncStats, error) {
	var plan []change
	stats := syncStats{}
	used := map[string]bool{}
	seenSources := map[string]bool{}
	for _, src := range sources {
		if src.Status == "cancelled" {
			continue
		}
		if len(src.Recurrence) > 0 || src.RecurringEventId != "" {
			return nil, stats, fmt.Errorf("recurring source event %s is unsupported", src.Id)
		}
		if src.Start == nil || src.End == nil {
			return nil, stats, fmt.Errorf("source event %s has no start/end", src.Id)
		}
		id := connpassID(src)
		identity := "source:" + src.Id
		if id != "" {
			identity = "connpass:" + id
		}
		if seenSources[identity] {
			return nil, stats, fmt.Errorf("duplicate source identity %s", identity)
		}
		seenSources[identity] = true
		var candidates []*calendar.Event
		for _, dst := range destinations {
			if dst.Status == "cancelled" || len(dst.Recurrence) > 0 || dst.RecurringEventId != "" {
				continue
			}
			legacy := private(dst, "sourceEventId") == src.Id && (private(dst, "sourceCalendarId") == "" || private(dst, "sourceCalendarId") == sourceCal)
			did := connpassID(dst)
			sameID := id != "" && did == id
			// Title/date adoption also covers older copies without identifiers, while
			// explicit conflicting connpass IDs can never be matched by their title.
			sameTitle := (id == "" || did == "" || id == did) && src.Summary == dst.Summary && sameTime(src.Start, dst.Start) && sameTime(src.End, dst.End)
			if legacy || sameID || sameTitle {
				candidates = append(candidates, dst)
			}
		}
		if len(candidates) > 1 {
			return nil, stats, fmt.Errorf("source event %s has %d destination matches; resolve duplicates before syncing", src.Id, len(candidates))
		}
		props := map[string]string{}
		var dst *calendar.Event
		if len(candidates) == 1 {
			dst = candidates[0]
			if used[dst.Id] {
				return nil, stats, fmt.Errorf("destination %s matches multiple source events", dst.Id)
			}
			used[dst.Id] = true
			if dst.ExtendedProperties != nil {
				for k, v := range dst.ExtendedProperties.Private {
					props[k] = v
				}
			}
		}
		props["sourceEventId"] = src.Id
		props["sourceCalendarId"] = sourceCal
		if id != "" {
			props["connpass_event_id"] = id
			props["connpass_sync_key"] = "connpass:" + id
		}
		target := &calendar.Event{Summary: src.Summary, Description: description(src, dst, id), Location: src.Location, Start: src.Start, End: src.End, ExtendedProperties: &calendar.EventExtendedProperties{Private: props}}
		if dst == nil {
			// Google permits base32hex IDs. Hex hashes are a subset; retries/concurrent
			// inserts use the same ID, so they cannot silently create a second copy.
			hash := sha256.Sum256([]byte(destCal + "\x00" + sourceCal + "\x00" + identity))
			target.Id = "cs" + hex.EncodeToString(hash[:])
			target.Reminders = &calendar.EventReminders{UseDefault: false, ForceSendFields: []string{"UseDefault"}}
			plan = append(plan, change{sourceID: src.Id, event: target})
			stats.Created++
		} else {
			if dst.Summary == target.Summary && dst.Description == target.Description && dst.Location == target.Location && sameTime(dst.Start, target.Start) && sameTime(dst.End, target.End) && dst.ExtendedProperties != nil && reflect.DeepEqual(dst.ExtendedProperties.Private, props) {
				stats.Unchanged++
				continue
			}
			target.ForceSendFields = []string{"Summary", "Description", "Location"}
			plan = append(plan, change{sourceID: src.Id, targetID: dst.Id, event: target})
			stats.Updated++
		}
	}
	return plan, stats, nil
}

func syncEvents(ctx context.Context, svc *calendar.Service, sourceCal, destCal string, since time.Time, dryRun bool) (syncStats, error) {
	if sourceCal == destCal {
		return syncStats{}, fmt.Errorf("source and destination must differ")
	}
	sources, err := listEvents(ctx, svc, sourceCal, since)
	if err != nil {
		return syncStats{}, fmt.Errorf("list source: %w", err)
	}
	destinations, err := listEvents(ctx, svc, destCal, time.Time{})
	if err != nil {
		return syncStats{}, fmt.Errorf("list destination: %w", err)
	}
	plan, stats, err := planEvents(sources, destinations, sourceCal, destCal)
	if err != nil {
		return stats, err
	}
	// Plan the complete run before any write. A failed lookup/ambiguous match
	// must never be treated as permission to insert. Absence never deletes.
	for _, c := range plan {
		action := "create"
		if c.targetID != "" {
			action = "update"
		}
		log.Printf("%s source_event=%s destination_event=%s dry_run=%t", action, c.sourceID, c.targetID, dryRun)
		if dryRun {
			continue
		}
		if c.targetID == "" {
			_, err = svc.Events.Insert(destCal, c.event).SendUpdates("none").Context(ctx).Do()
			if apiErr, ok := err.(*googleapi.Error); ok && apiErr.Code == 409 {
				return stats, fmt.Errorf("event already exists; rerun to reconcile: %w", err)
			}
		} else {
			_, err = svc.Events.Patch(destCal, c.targetID, c.event).SendUpdates("none").Context(ctx).Do()
		}
		if err != nil {
			return stats, fmt.Errorf("%s source event %s: %w", action, c.sourceID, err)
		}
	}
	return stats, nil
}
