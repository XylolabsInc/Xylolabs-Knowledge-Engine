package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/xylolabsinc/xylolabs-kb/internal/extractor"
	"github.com/xylolabsinc/xylolabs-kb/internal/kb"
)

const (
	notionAPIBase    = "https://api.notion.com/v1"
	notionAPIVersion = "2022-06-28"
)

// Connector handles Notion API integration.
type Connector struct {
	apiKey     string
	rootPages  []string
	httpClient *http.Client
	engine     *kb.Engine
	store      kb.Storage
	logger     *slog.Logger
	limiter    *rate.Limiter
}

// NewConnector creates a Notion connector.
func NewConnector(apiKey string, rootPages []string, engine *kb.Engine, store kb.Storage, logger *slog.Logger) *Connector {
	return &Connector{
		apiKey:    apiKey,
		rootPages: rootPages,
		httpClient: extractor.NewRestrictedHTTPClient(30 * time.Second),
		engine:  engine,
		store:   store,
		logger:  logger.With("component", "notion-connector"),
		limiter: rate.NewLimiter(rate.Limit(3), 3),
	}
}

// Name returns the source identifier.
func (c *Connector) Name() kb.Source {
	return kb.SourceNotion
}

// Start is a no-op for Notion — poll-based only.
func (c *Connector) Start(done <-chan struct{}) error {
	c.logger.Info("notion connector started (poll-based)")
	<-done
	return nil
}

// Stop gracefully shuts down the connector.
func (c *Connector) Stop() error {
	c.logger.Info("notion connector stopped")
	return nil
}

const maxRecursionDepth = 10

const maxAPIResponseSize = 50 << 20 // 50 MB

// Sync fetches all pages from Notion, recursively traversing from root pages.
func (c *Connector) Sync(ctx context.Context) error {
	c.logger.Info("starting notion sync")

	visited := make(map[string]bool)
	var indexed int

	if len(c.rootPages) > 0 {
		// Recursive traversal from root pages — discovers all child pages and databases.
		for _, rootID := range c.rootPages {
			count, err := c.syncPageRecursive(ctx, rootID, visited, 0)
			if err != nil {
				c.logger.Warn("failed to sync root page tree", "page_id", rootID, "error", err)
			}
			indexed += count
		}
	} else {
		// Fallback: use search API.
		pages, err := c.searchPages(ctx, nil)
		if err != nil {
			return fmt.Errorf("search pages: %w", err)
		}
		c.logger.Info("found notion pages", "count", len(pages))
		for _, page := range pages {
			blocks, err := c.getPageBlocks(ctx, page.ID)
			if err != nil {
				c.logger.Warn("failed to get blocks", "page_id", page.ID, "error", err)
				continue
			}
			doc := ConvertPage(page, blocks)
			if err := c.engine.Index(ctx, doc); err != nil {
				c.logger.Warn("failed to index page", "page_id", page.ID, "error", err)
				continue
			}
			indexed++
		}
	}

	now := time.Now().UTC()
	newState := kb.SyncState{
		Source:     kb.SourceNotion,
		LastSyncAt: now,
		Metadata:   map[string]string{"pages_synced": fmt.Sprintf("%d", indexed)},
	}
	if err := c.store.SetSyncState(newState); err != nil {
		return fmt.Errorf("set notion sync state: %w", err)
	}

	c.logger.Info("notion sync complete", "pages", indexed)
	return nil
}

// syncPageRecursive fetches a page, indexes it, then recurses into child pages and databases.
func (c *Connector) syncPageRecursive(ctx context.Context, pageID string, visited map[string]bool, depth int) (int, error) {
	if depth > maxRecursionDepth {
		return 0, nil
	}

	// Normalize page ID (remove hyphens for dedup).
	normalID := strings.ReplaceAll(pageID, "-", "")
	if visited[normalID] {
		return 0, nil
	}
	visited[normalID] = true

	// Fetch page metadata.
	page, err := c.getPage(ctx, pageID)
	if err != nil {
		return 0, fmt.Errorf("get page %s: %w", pageID, err)
	}

	// Fetch blocks.
	blocks, err := c.getPageBlocks(ctx, pageID)
	if err != nil {
		return 0, fmt.Errorf("get blocks for %s: %w", pageID, err)
	}

	// Index this page.
	doc := ConvertPage(page, blocks)
	if err := c.engine.Index(ctx, doc); err != nil {
		c.logger.Warn("failed to index page", "page_id", pageID, "title", page.Title, "error", err)
	}
	count := 1

	c.logger.Info("indexed notion page", "page_id", pageID, "title", page.Title, "depth", depth)

	// Find and recurse into child pages.
	for _, childID := range FindChildPages(blocks) {
		childCount, err := c.syncPageRecursive(ctx, childID, visited, depth+1)
		if err != nil {
			c.logger.Warn("failed to sync child page", "child_id", childID, "parent_id", pageID, "error", err)
			continue
		}
		count += childCount
	}

	// Query and recurse into child databases.
	for _, dbID := range FindChildDatabases(blocks) {
		dbCount, err := c.syncDatabase(ctx, dbID, visited, depth+1)
		if err != nil {
			c.logger.Warn("failed to sync child database", "db_id", dbID, "parent_id", pageID, "error", err)
			continue
		}
		count += dbCount
	}

	return count, nil
}

// getPage fetches a single Notion page by ID.
func (c *Connector) getPage(ctx context.Context, pageID string) (NotionPage, error) {
	resp, err := c.apiRequest(ctx, "GET", "/pages/"+pageID, nil)
	if err != nil {
		return NotionPage{}, fmt.Errorf("get page: %w", err)
	}
	return c.parsePage(resp), nil
}

// syncDatabase queries a Notion database and indexes all its pages.
func (c *Connector) syncDatabase(ctx context.Context, dbID string, visited map[string]bool, depth int) (int, error) {
	if depth > maxRecursionDepth {
		return 0, nil
	}

	var count int
	var startCursor *string

	for {
		body := map[string]any{
			"page_size": 100,
		}
		if startCursor != nil {
			body["start_cursor"] = *startCursor
		}

		resp, err := c.apiRequest(ctx, "POST", "/databases/"+dbID+"/query", body)
		if err != nil {
			return count, fmt.Errorf("query database %s: %w", dbID, err)
		}

		results, _ := resp["results"].([]any)
		hasMore, _ := resp["has_more"].(bool)
		nextCursor, _ := resp["next_cursor"].(string)

		for _, r := range results {
			page, ok := r.(map[string]any)
			if !ok {
				continue
			}
			pageID := stringVal(page, "id")
			if pageID == "" {
				continue
			}

			childCount, err := c.syncPageRecursive(ctx, pageID, visited, depth+1)
			if err != nil {
				c.logger.Warn("failed to sync database page", "page_id", pageID, "db_id", dbID, "error", err)
				continue
			}
			count += childCount
		}

		if !hasMore || nextCursor == "" {
			break
		}
		startCursor = &nextCursor
	}

	return count, nil
}

// NotionPage represents a Notion page from the API.
type NotionPage struct {
	ID             string
	Title          string
	URL            string
	CreatedTime    time.Time
	LastEditedTime time.Time
	CreatedBy      string
	Properties     map[string]any
}

// NotionBlock represents a Notion block.
type NotionBlock struct {
	ID       string
	Type     string
	HasChild bool
	Content  string
	Children []NotionBlock
}

func (c *Connector) searchPages(ctx context.Context, lastEdited *time.Time) ([]NotionPage, error) {
	var allPages []NotionPage
	var startCursor *string

	for {
		body := map[string]any{
			"page_size": 100,
			"filter": map[string]any{
				"value":    "page",
				"property": "object",
			},
			"sort": map[string]any{
				"direction": "descending",
				"timestamp": "last_edited_time",
			},
		}
		if startCursor != nil {
			body["start_cursor"] = *startCursor
		}

		resp, err := c.apiRequest(ctx, "POST", "/search", body)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}

		results, _ := resp["results"].([]any)
		hasMore, _ := resp["has_more"].(bool)
		nextCursor, _ := resp["next_cursor"].(string)

		for _, r := range results {
			page, ok := r.(map[string]any)
			if !ok {
				continue
			}

			np := c.parsePage(page)

			if lastEdited != nil && np.LastEditedTime.Before(*lastEdited) {
				return allPages, nil
			}

			allPages = append(allPages, np)
		}

		if !hasMore || nextCursor == "" {
			break
		}
		startCursor = &nextCursor
	}

	return allPages, nil
}

func (c *Connector) getPageBlocks(ctx context.Context, pageID string) ([]NotionBlock, error) {
	return c.getBlockChildren(ctx, pageID, 0)
}

func (c *Connector) getBlockChildren(ctx context.Context, blockID string, depth int) ([]NotionBlock, error) {
	if depth > 5 {
		return nil, nil // prevent infinite recursion
	}

	var allBlocks []NotionBlock
	var startCursor *string

	for {
		path := fmt.Sprintf("/blocks/%s/children?page_size=100", blockID)
		if startCursor != nil {
			path += "&start_cursor=" + *startCursor
		}

		resp, err := c.apiRequest(ctx, "GET", path, nil)
		if err != nil {
			return nil, fmt.Errorf("get block children: %w", err)
		}

		results, _ := resp["results"].([]any)
		hasMore, _ := resp["has_more"].(bool)
		nextCursor, _ := resp["next_cursor"].(string)

		for _, r := range results {
			block, ok := r.(map[string]any)
			if !ok {
				continue
			}

			nb := parseBlock(block)

			hasChildren, _ := block["has_children"].(bool)
			// Don't recurse into child pages/databases — they are indexed as separate documents.
			if hasChildren && nb.Type != "child_page" && nb.Type != "child_database" {
				children, err := c.getBlockChildren(ctx, nb.ID, depth+1)
				if err != nil {
					c.logger.Debug("failed to get nested blocks", "block_id", nb.ID, "error", err)
				} else {
					nb.Children = children
				}
			}

			allBlocks = append(allBlocks, nb)
		}

		if !hasMore || nextCursor == "" {
			break
		}
		startCursor = &nextCursor
	}

	return allBlocks, nil
}

func (c *Connector) parsePage(page map[string]any) NotionPage {
	np := NotionPage{
		ID:  stringVal(page, "id"),
		URL: stringVal(page, "url"),
	}

	if ct, ok := page["created_time"].(string); ok {
		np.CreatedTime, _ = time.Parse(time.RFC3339, ct)
	}
	if et, ok := page["last_edited_time"].(string); ok {
		np.LastEditedTime, _ = time.Parse(time.RFC3339, et)
	}
	if cb, ok := page["created_by"].(map[string]any); ok {
		np.CreatedBy = stringVal(cb, "name")
		if np.CreatedBy == "" {
			np.CreatedBy = stringVal(cb, "id")
		}
	}

	// Extract title from properties
	if props, ok := page["properties"].(map[string]any); ok {
		np.Properties = props
		np.Title = extractTitle(props)
	}

	return np
}

func extractTitle(props map[string]any) string {
	for _, v := range props {
		prop, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if prop["type"] != "title" {
			continue
		}
		titleArr, ok := prop["title"].([]any)
		if !ok || len(titleArr) == 0 {
			continue
		}
		var title string
		for _, t := range titleArr {
			if rt, ok := t.(map[string]any); ok {
				if pt, ok := rt["plain_text"].(string); ok {
					title += pt
				}
			}
		}
		return title
	}
	return ""
}

func (c *Connector) apiRequest(ctx context.Context, method, path string, body any) (map[string]any, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("rate limit: %w", err)
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, notionAPIBase+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Notion-Version", notionAPIVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		errBody := string(respData)
		if len(errBody) > 512 {
			errBody = errBody[:512] + "... (truncated)"
		}
		return nil, fmt.Errorf("notion API error %d: %s", resp.StatusCode, errBody)
	}

	var result map[string]any
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return result, nil
}

func stringVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}
