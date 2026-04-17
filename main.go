package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"gopkg.in/yaml.v3"
)

type appConfig struct {
	RedirectURL           string `yaml:"redirect_url"`
	SourceCalendarID      string `yaml:"source_calendar_id"`
	DestinationCalendarID string `yaml:"destination_calendar_id"`
}

func loadConfig(path string) (*appConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c appConfig
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.RedirectURL == "" {
		return nil, fmt.Errorf("%s: redirect_url is required", path)
	}
	if c.SourceCalendarID == "" {
		return nil, fmt.Errorf("%s: source_calendar_id is required", path)
	}
	if c.DestinationCalendarID == "" {
		return nil, fmt.Errorf("%s: destination_calendar_id is required", path)
	}
	return &c, nil
}

// getClient retrieves a token, saves it, and returns an HTTP client.
func getClient(config *oauth2.Config) *http.Client {
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// tokenFromFile reads a token from a file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// getTokenFromWeb starts a Gin server with OAuth routes and returns a token.
// Does NOT override the RedirectURL in config; uses what’s in credentials.json.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	codeCh := make(chan string)

	r := gin.Default()
	r.GET("/setup/auth", func(c *gin.Context) {
		c.Redirect(http.StatusFound, authURL)
	})
	r.GET("/setup/auth/callback", func(c *gin.Context) {
		if errParam := c.Query("error"); errParam != "" {
			c.String(http.StatusBadRequest, "Authentication failed: %s", errParam)
			codeCh <- ""
			return
		}
		code := c.Query("code")
		c.String(http.StatusOK, "Authorization received; you can close this window.")
		codeCh <- code
	})

	srv := &http.Server{Addr: ":1853", Handler: r}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	fmt.Println("Open http://localhost:1853/setup/auth in your browser to authorize the app.")
	code := <-codeCh

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)

	if code == "" {
		log.Fatalf("No code received; aborting.")
	}
	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		log.Fatalf("Token exchange error: %v", err)
	}
	return tok
}

// saveToken writes a token to a file.
func saveToken(path string, token *oauth2.Token) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Unable to create %s: %v", path, err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

// checkAuthorization tests the stored token by fetching calendar list.
func checkAuthorization(srv *calendar.Service) {
	if _, err := srv.CalendarList.List().MaxResults(1).Do(); err != nil {
		fmt.Println("Invalid token; remove token.json and rerun to reauthorize.")
		os.Exit(1)
	}
}

// syncEvents copies or updates events from sourceCal to destCal since `since`.
func syncEvents(srv *calendar.Service, sourceCal, destCal string, since time.Time) {
	evList, err := srv.Events.List(sourceCal).
		ShowDeleted(false).
		SingleEvents(true).
		UpdatedMin(since.Format(time.RFC3339)).
		Do()
	if err != nil {
		log.Printf("Error listing source events: %v", err)
		return
	}

	for _, src := range evList.Items {
		origID := src.Id

		// look for existing by extended property
		dq, err := srv.Events.List(destCal).
			PrivateExtendedProperty("sourceEventId=" + origID).
			SingleEvents(true).
			Do()
		if err == nil && len(dq.Items) > 0 {
			existing := dq.Items[0]

			// detect changes
			needsUpdate := false
			patched := &calendar.Event{}
			if src.Summary != existing.Summary {
				patched.Summary = src.Summary
				needsUpdate = true
			}
			if src.Description != existing.Description {
				patched.Description = src.Description
				needsUpdate = true
			}
			if needsUpdate {
				log.Printf("Updating event %s: %s", origID, src.Summary)
				// preserve ext prop
				if existing.ExtendedProperties == nil {
					existing.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{}}
				}
				existing.ExtendedProperties.Private["sourceEventId"] = origID
				patched.ExtendedProperties = existing.ExtendedProperties
				if _, err := srv.Events.Patch(destCal, existing.Id, patched).Do(); err != nil {
					log.Printf("Failed to patch event %s: %v", existing.Id, err)
				}
			}
		} else {
			// insert new
			src.Id = ""
			if src.ExtendedProperties == nil {
				src.ExtendedProperties = &calendar.EventExtendedProperties{Private: map[string]string{}}
			}
			src.ExtendedProperties.Private["sourceEventId"] = origID
			log.Printf("Adding event %s: %q", origID, src.Summary)
			if _, err := srv.Events.Insert(destCal, src).Do(); err != nil {
				log.Printf("Failed to insert event for %s: %v", origID, err)
			}
		}
	}
}

func main() {
	// Read credentials
	creds, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Read credentials.json: %v", err)
	}
	config, err := google.ConfigFromJSON(creds, calendar.CalendarScope)
	if err != nil {
		log.Fatalf("Parse credentials: %v", err)
	}

	cfg, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Load config: %v", err)
	}
	config.RedirectURL = cfg.RedirectURL

	// OAuth2 client
	client := getClient(config)
	svc, err := calendar.New(client)
	if err != nil {
		log.Fatalf("Create Calendar service: %v", err)
	}
	checkAuthorization(svc)

	sinceFlag := flag.String("since", "", "Only sync events updated since this RFC3339 timestamp (default: all)")
	flag.Parse()

	since := time.Time{}
	if *sinceFlag != "" {
		parsed, err := time.Parse(time.RFC3339, *sinceFlag)
		if err != nil {
			log.Fatalf("Bad --since timestamp: %v", err)
		}
		since = parsed
	}

	srcID := cfg.SourceCalendarID
	dstID := cfg.DestinationCalendarID
	log.Printf("Syncing from %q to %q", srcID, dstID)
	syncEvents(svc, srcID, dstID, since)
	log.Println("Sync completed.")
}
