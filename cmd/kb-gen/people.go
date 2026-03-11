package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
	admin "google.golang.org/api/admin/directory/v1"
	"google.golang.org/api/option"
)

// Person represents a Google Workspace user for KB generation.
type Person struct {
	Email        string   `json:"email"`
	FullName     string   `json:"full_name"`
	GivenName    string   `json:"given_name"`
	FamilyName   string   `json:"family_name"`
	Title        string   `json:"title,omitempty"`
	Department   string   `json:"department,omitempty"`
	Phone        string   `json:"phone,omitempty"`
	OrgUnit      string   `json:"org_unit,omitempty"`
	IsAdmin      bool     `json:"is_admin"`
	Aliases      []string `json:"aliases,omitempty"`
	CreationTime string   `json:"creation_time,omitempty"`
	LastLogin    string   `json:"last_login,omitempty"`
	PhotoURL     string   `json:"photo_url,omitempty"`
}

func fetchAndWritePeople(credsFile, impersonateEmail, domain, kbDir string, dryRun bool, logger *slog.Logger) error {
	ctx := context.Background()

	logger.Info("fetching google workspace directory",
		"domain", domain,
		"impersonate", impersonateEmail,
	)

	// Read service account credentials
	credBytes, err := os.ReadFile(credsFile)
	if err != nil {
		return fmt.Errorf("read credentials: %w", err)
	}

	// Create Admin SDK service with domain-wide delegation
	// The Admin SDK requires impersonation of a Workspace admin user
	scopes := []string{admin.AdminDirectoryUserReadonlyScope}

	jwtConfig, err := google.JWTConfigFromJSON(credBytes, scopes...)
	if err != nil {
		return fmt.Errorf("parse service account key: %w", err)
	}
	if impersonateEmail != "" {
		jwtConfig.Subject = impersonateEmail
	}

	httpClient := jwtConfig.Client(ctx)

	adminService, err := admin.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("create admin service: %w", err)
	}

	// Fetch all users from the domain
	var people []Person
	pageToken := ""
	for {
		req := adminService.Users.List().
			Domain(domain).
			MaxResults(500).
			OrderBy("email").
			Projection("full")
		if pageToken != "" {
			req = req.PageToken(pageToken)
		}

		resp, err := req.Do()
		if err != nil {
			return fmt.Errorf("list users: %w", err)
		}

		for _, u := range resp.Users {
			p := Person{
				Email:        u.PrimaryEmail,
				FullName:     u.Name.FullName,
				GivenName:    u.Name.GivenName,
				FamilyName:   u.Name.FamilyName,
				IsAdmin:      u.IsAdmin,
				OrgUnit:      u.OrgUnitPath,
				CreationTime: u.CreationTime,
				LastLogin:    u.LastLoginTime,
			}

			// Extract organization info (title, department)
			if u.Organizations != nil {
				orgsJSON, _ := json.Marshal(u.Organizations)
				var orgs []struct {
					Title      string `json:"title"`
					Department string `json:"department"`
					Primary    bool   `json:"primary"`
				}
				if json.Unmarshal(orgsJSON, &orgs) == nil {
					for _, org := range orgs {
						if org.Title != "" && p.Title == "" {
							p.Title = org.Title
						}
						if org.Department != "" && p.Department == "" {
							p.Department = org.Department
						}
					}
				}
			}

			// Extract phone numbers
			if u.Phones != nil {
				phonesJSON, _ := json.Marshal(u.Phones)
				var phones []struct {
					Value   string `json:"value"`
					Primary bool   `json:"primary"`
				}
				if json.Unmarshal(phonesJSON, &phones) == nil {
					for _, phone := range phones {
						if phone.Value != "" {
							p.Phone = phone.Value
							if phone.Primary {
								break
							}
						}
					}
				}
			}

			// Extract aliases
			if len(u.Aliases) > 0 {
				p.Aliases = u.Aliases
			}

			// Extract photo URL
			if u.ThumbnailPhotoUrl != "" {
				p.PhotoURL = u.ThumbnailPhotoUrl
			}

			people = append(people, p)
		}

		logger.Info("fetched users page", "count", len(resp.Users), "total_so_far", len(people))

		pageToken = resp.NextPageToken
		if pageToken == "" {
			break
		}
	}

	logger.Info("fetched all workspace users", "total", len(people))

	// Generate person knowledge files
	peopleDir := filepath.Join(kbDir, "people")

	// Generate individual person files
	filesWritten := 0
	for _, p := range people {
		slug := emailToSlug(p.Email)
		relPath := filepath.Join("people", slug+".md")
		content := renderPersonMarkdown(p)

		if dryRun {
			fmt.Printf("[dry-run] Would write: %s (%d bytes)\n", relPath, len(content))
			continue
		}

		fullPath := filepath.Join(kbDir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			logger.Error("failed to create directory", "path", filepath.Dir(fullPath), "error", err)
			continue
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			logger.Error("failed to write person file", "path", relPath, "error", err)
			continue
		}
		logger.Info("wrote person file", "path", relPath)
		filesWritten++
	}

	// Generate people index
	indexContent := renderPeopleIndex(people, domain)
	indexPath := filepath.Join(peopleDir, "README.md")
	if dryRun {
		fmt.Printf("[dry-run] Would write: people/README.md (%d bytes)\n", len(indexContent))
	} else {
		if err := os.MkdirAll(peopleDir, 0o755); err != nil {
			return fmt.Errorf("create people dir: %w", err)
		}
		if err := os.WriteFile(indexPath, []byte(indexContent), 0o644); err != nil {
			return fmt.Errorf("write people index: %w", err)
		}
		filesWritten++
		logger.Info("wrote people index", "path", "people/README.md")
	}

	logger.Info("people knowledge generation complete",
		"files_written", filesWritten,
		"total_people", len(people),
		"dry_run", dryRun,
	)

	return nil
}

func emailToSlug(email string) string {
	// Take the local part before @
	parts := strings.SplitN(email, "@", 2)
	slug := parts[0]
	// Replace dots and special chars with hyphens
	slug = strings.ReplaceAll(slug, ".", "-")
	slug = strings.ReplaceAll(slug, "_", "-")
	return strings.ToLower(slug)
}

func renderPersonMarkdown(p Person) string {
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("title: \"%s\"\n", p.FullName))
	sb.WriteString(fmt.Sprintf("email: \"%s\"\n", p.Email))
	if p.Title != "" {
		sb.WriteString(fmt.Sprintf("role: \"%s\"\n", p.Title))
	}
	if p.Department != "" {
		sb.WriteString(fmt.Sprintf("department: \"%s\"\n", p.Department))
	}
	sb.WriteString(fmt.Sprintf("date: \"%s\"\n", time.Now().Format("2006-01-02")))
	sb.WriteString("source: google-workspace\n")
	sb.WriteString("type: person\n")
	sb.WriteString("---\n\n")

	sb.WriteString(fmt.Sprintf("# %s\n\n", p.FullName))

	sb.WriteString("## Contact Information\n\n")
	sb.WriteString(fmt.Sprintf("- **Email**: %s\n", p.Email))
	if len(p.Aliases) > 0 {
		sb.WriteString(fmt.Sprintf("- **Aliases**: %s\n", strings.Join(p.Aliases, ", ")))
	}
	if p.Phone != "" {
		sb.WriteString(fmt.Sprintf("- **Phone**: %s\n", p.Phone))
	}
	sb.WriteString("\n")

	sb.WriteString("## Role\n\n")
	if p.Title != "" {
		sb.WriteString(fmt.Sprintf("- **Title**: %s\n", p.Title))
	}
	if p.Department != "" {
		sb.WriteString(fmt.Sprintf("- **Department**: %s\n", p.Department))
	}
	if p.OrgUnit != "" && p.OrgUnit != "/" {
		sb.WriteString(fmt.Sprintf("- **Organization Unit**: %s\n", p.OrgUnit))
	}
	if p.IsAdmin {
		sb.WriteString("- **Admin**: Yes\n")
	}
	sb.WriteString("\n")

	if p.CreationTime != "" {
		sb.WriteString("## Account\n\n")
		sb.WriteString(fmt.Sprintf("- **Created**: %s\n", p.CreationTime))
		if p.LastLogin != "" {
			sb.WriteString(fmt.Sprintf("- **Last Login**: %s\n", p.LastLogin))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func renderPeopleIndex(people []Person, domain string) string {
	var sb strings.Builder

	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("title: \"Team Directory - %s\"\n", domain))
	sb.WriteString(fmt.Sprintf("date: \"%s\"\n", time.Now().Format("2006-01-02")))
	sb.WriteString("source: google-workspace\n")
	sb.WriteString("type: index\n")
	sb.WriteString("---\n\n")

	sb.WriteString(fmt.Sprintf("# Team Directory (%s)\n\n", domain))
	sb.WriteString(fmt.Sprintf("Total members: %d\n\n", len(people)))

	sb.WriteString("| Name | Email | Title | Department |\n")
	sb.WriteString("|------|-------|-------|------------|\n")

	for _, p := range people {
		title := p.Title
		if title == "" {
			title = "-"
		}
		dept := p.Department
		if dept == "" {
			dept = "-"
		}
		sb.WriteString(fmt.Sprintf("| [%s](%s.md) | %s | %s | %s |\n",
			p.FullName, emailToSlug(p.Email), p.Email, title, dept))
	}

	sb.WriteString("\n")
	return sb.String()
}
