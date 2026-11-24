package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func fixture(id string) *calendar.Event {
	return &calendar.Event{Id: id, Summary: "Connpass event", Description: "Details\nhttps://group.connpass.com/event/12345/", Location: "Tokyo", Start: &calendar.EventDateTime{DateTime: "2026-11-01T19:00:00+09:00"}, End: &calendar.EventDateTime{DateTime: "2026-11-01T21:00:00+09:00"}}
}

func TestRestoredAndLegacyAdoption(t *testing.T) {
	for _, kind := range []string{"restored", "legacy", "title"} {
		t.Run(kind, func(t *testing.T) {
			src, dst := fixture("feed-id"), fixture("existing-id")
			dst.Description = "Personal description"
			if kind == "restored" {
				dst.Description = "Archive<br>connpass_event_id: 12345<br>connpass_sync_key: connpass:12345<br>connpass_import_batch: 2026-11-17-history"
			}
			if kind == "legacy" {
				dst.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{"sourceEventId": "feed-id", "custom": "keep"}}
			}
			plan, stats, err := planEvents([]*calendar.Event{src}, []*calendar.Event{dst}, "source", "dest")
			if err != nil || len(plan) != 1 || stats.Created != 0 || plan[0].targetID != "existing-id" {
				t.Fatalf("wrong adoption: %+v %+v %v", plan, stats, err)
			}
			if kind == "restored" && plan[0].event.Description != dst.Description {
				t.Fatal("archive description overwritten")
			}
			if kind == "legacy" && private(plan[0].event, "custom") != "keep" {
				t.Fatal("custom property lost")
			}
			dst = plan[0].event
			dst.Id = "existing-id"
			plan, stats, err = planEvents([]*calendar.Event{src}, []*calendar.Event{dst}, "source", "dest")
			if err != nil || len(plan) != 0 || stats.Unchanged != 1 {
				t.Fatalf("second run not idempotent: %+v %v", stats, err)
			}
		})
	}
}

func TestIdentityAndPreservation(t *testing.T) {
	e := fixture("x")
	e.Description = "Related: https://connpass.com/event/999/\n\nhttps://group.connpass.com/event/12345/"
	if connpassID(e) != "12345" {
		t.Fatal("source URL must win over related link")
	}
	e.Description = "<p>connpass_event_id: 12345</p><a href='https://connpass.com/event/999/'>Related</a>"
	if connpassID(e) != "12345" {
		t.Fatal("HTML marker not recognized")
	}
	plan, stats, err := planEvents(nil, []*calendar.Event{e}, "source", "dest")
	if err != nil || len(plan) != 0 || stats.Created != 0 {
		t.Fatal("source disappearance must preserve history")
	}
	src := fixture("source-id")
	src.Attendees = []*calendar.EventAttendee{{Email: "someone@example.com"}}
	src.Recurrence = nil
	plan, _, err = planEvents([]*calendar.Event{src}, nil, "source", "dest")
	if err != nil || len(plan) != 1 {
		t.Fatal(err)
	}
	if len(plan[0].event.Attendees) != 0 || plan[0].event.Reminders.UseDefault {
		t.Fatal("copied guest or reminder side effects")
	}
	again, _, _ := planEvents([]*calendar.Event{src}, nil, "source", "dest")
	if plan[0].event.Id != again[0].event.Id {
		t.Fatal("unstable insertion ID")
	}
}

func TestTimeChangesAmbiguityAndCancellation(t *testing.T) {
	src, dst := fixture("feed"), fixture("existing")
	dst.Start = &calendar.EventDateTime{DateTime: "2026-11-01T18:00:00+09:00"}
	dst.Location = "Old venue"
	plan, _, err := planEvents([]*calendar.Event{src}, []*calendar.Event{dst}, "source", "dest")
	if err != nil || len(plan) != 1 || plan[0].targetID != "existing" || !sameTime(plan[0].event.Start, src.Start) || plan[0].event.Location != "Tokyo" {
		t.Fatal("reschedule not updated", err)
	}
	if _, _, err = planEvents([]*calendar.Event{src}, []*calendar.Event{dst, fixture("duplicate")}, "source", "dest"); err == nil {
		t.Fatal("ambiguous matches accepted")
	}
	src.Status = "cancelled"
	plan, _, err = planEvents([]*calendar.Event{src}, []*calendar.Event{dst}, "source", "dest")
	if err != nil || len(plan) != 0 {
		t.Fatal("canceled source must not change archive")
	}
	src.Status = "confirmed"
	src.Start = &calendar.EventDateTime{Date: "2026-11-01"}
	src.End = &calendar.EventDateTime{Date: "2026-11-03"}
	plan, _, err = planEvents([]*calendar.Event{src}, nil, "source", "dest")
	if err != nil || plan[0].event.End.Date != "2026-11-03" {
		t.Fatal("all-day boundary changed")
	}
}

func TestAPIReadFailuresPaginationAndDryRun(t *testing.T) {
	for _, failure := range []bool{false, true} {
		writes, pages := 0, 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" {
				writes++
				t.Error("dry run attempted write")
				return
			}
			if strings.Contains(r.URL.Path, "/source/") {
				if r.URL.Query().Get("updatedMin") != "" {
					t.Error("zero --since sent to API")
				}
				pages++
				if r.URL.Query().Get("pageToken") == "" {
					json.NewEncoder(w).Encode(calendar.Events{NextPageToken: "next"})
					return
				}
				json.NewEncoder(w).Encode(calendar.Events{Items: []*calendar.Event{fixture("source-event")}})
				return
			}
			if failure {
				http.Error(w, "lookup failure", 500)
				return
			}
			json.NewEncoder(w).Encode(calendar.Events{})
		}))
		svc, err := calendar.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
		if err != nil {
			t.Fatal(err)
		}
		stats, err := syncEvents(context.Background(), svc, "source", "dest", time.Time{}, true)
		server.Close()
		if failure && err == nil {
			t.Fatal("lookup failure ignored")
		}
		if !failure && (err != nil || stats.Created != 1) {
			t.Fatalf("unexpected plan %+v %v", stats, err)
		}
		if writes != 0 || pages != 2 {
			t.Fatalf("writes=%d source pages=%d", writes, pages)
		}
	}
}
