package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	appStyle = lipgloss.NewStyle()

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFFDF5")).
			Background(lipgloss.Color("#25A065")).
			Padding(0, 1)

	dayStyle = lipgloss.NewStyle().
			Width(80).
			Height(25).
			Padding(1).
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("62"))

	focusedDayStyle = lipgloss.NewStyle().
			Width(80).
			Height(25).
			Padding(1).
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("205"))

	statusMessageStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#04B575", Dark: "#04B575"}).
				Render
)

type animeItem struct {
	anime AnimeTimetable
}

func (i animeItem) Title() string {
	title := strings.TrimSpace(i.anime.Title)

	// Final fallback if everything is empty
	if title == "" {
		title = "Unknown Title"
	}

	maxWidth := 50

	if len(title) <= maxWidth {
		return fmt.Sprintf("%-*s", maxWidth, title)
	}

	// Wrap longer titles to next line
	var lines []string
	remaining := title

	for len(remaining) > 0 {
		if len(remaining) <= maxWidth {
			lines = append(lines, fmt.Sprintf("%-*s", maxWidth, remaining))
			break
		}

		// Find a good break point (space, dash, etc.)
		breakPoint := maxWidth
		for j := maxWidth - 1; j >= maxWidth/2 && j < len(remaining); j-- {
			if remaining[j] == ' ' || remaining[j] == '-' || remaining[j] == ':' {
				breakPoint = j
				break
			}
		}

		lines = append(lines, fmt.Sprintf("%-*s", maxWidth, remaining[:breakPoint]))
		remaining = strings.TrimSpace(remaining[breakPoint:])
	}

	return strings.Join(lines, "\n")
}

func (i animeItem) Description() string {
	return fmt.Sprintf("Episode %d • %s • %s",
		i.anime.EpisodeNumber,
		i.anime.EpisodeDate.Format("Jan 2, 15:04"),
		i.anime.AirType)
}

// Fuzzy search scoring function
func fuzzyScore(query, text string) int {
	query = strings.ToLower(query)
	text = strings.ToLower(text)

	if query == "" {
		return 1000 // High score for empty query
	}

	if strings.Contains(text, query) {
		// Exact substring match gets high score
		return 100 + (100 - len(query))
	}

	// Calculate fuzzy match score
	score := 0
	queryRunes := []rune(query)
	textRunes := []rune(text)

	queryIndex := 0
	for textIndex, textRune := range textRunes {
		if queryIndex < len(queryRunes) && unicode.ToLower(textRune) == unicode.ToLower(queryRunes[queryIndex]) {
			// Match found - give points based on position and consecutiveness
			positionBonus := max(0, 50-textIndex)
			score += 10 + positionBonus
			queryIndex++
		}
	}

	// Bonus for matching all characters
	if queryIndex == len(queryRunes) {
		score += 20
	}

	return score
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (i animeItem) FilterValue() string {
	// Use title for fuzzy matching
	return i.anime.Title
}

// Custom filter function for fuzzy search
func fuzzyFilter(term string, targets []string) []list.Rank {
	var ranks []list.Rank

	for i, target := range targets {
		score := fuzzyScore(term, target)
		if score > 0 {
			ranks = append(ranks, list.Rank{
				Index:          i,
				MatchedIndexes: nil, // We'll let the list handle highlighting
			})
		}
	}

	// Sort by score (higher scores first)
	for i := 0; i < len(ranks)-1; i++ {
		for j := i + 1; j < len(ranks); j++ {
			scoreI := fuzzyScore(term, targets[ranks[i].Index])
			scoreJ := fuzzyScore(term, targets[ranks[j].Index])
			if scoreI < scoreJ {
				ranks[i], ranks[j] = ranks[j], ranks[i]
			}
		}
	}

	return ranks
}

type listKeyMap struct {
	toggleTitleBar   key.Binding
	toggleStatusBar  key.Binding
	togglePagination key.Binding
	toggleHelpMenu   key.Binding
}

func newListKeyMap() *listKeyMap {
	return &listKeyMap{
		toggleTitleBar: key.NewBinding(
			key.WithKeys("T"),
			key.WithHelp("T", "toggle title"),
		),
		toggleStatusBar: key.NewBinding(
			key.WithKeys("S"),
			key.WithHelp("S", "toggle status"),
		),
		togglePagination: key.NewBinding(
			key.WithKeys("P"),
			key.WithHelp("P", "toggle pagination"),
		),
		toggleHelpMenu: key.NewBinding(
			key.WithKeys("H"),
			key.WithHelp("H", "toggle help"),
		),
	}
}

type fetchTimetableMsg []AnimeTimetable
type errMsg error

type appState int

const (
	stateLoading appState = iota
	stateWeekly
)

type weeklyModel struct {
	state        appState
	spinner      spinner.Model
	allAnime     []animeItem
	list         list.Model
	focusedDay   time.Weekday
	keys         *listKeyMap
	delegateKeys *delegateKeyMap
	err          error
	width        int
	height       int
}

func initialModel(apiToken string) weeklyModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	// Set focused day to current day
	currentDay := time.Now().Weekday()

	delegateKeys := newDelegateKeyMap()

	return weeklyModel{
		state:      stateLoading,
		spinner:    s,
		allAnime:   []animeItem{},
		list:       list.New([]list.Item{}, newItemDelegate(delegateKeys), 80, 24),
		focusedDay: currentDay,
		width:      80,
		height:     24,
	}
}

func (m weeklyModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		fetchTimetableCmd,
		tea.EnterAltScreen,
	)
}

func getSystemTimezone() string {
	// Method 1: Try to get timezone from environment variable
	if timezone := os.Getenv("TZ"); timezone != "" {
		return timezone
	}

	// Method 2: Try to resolve symlink /etc/localtime (Linux/macOS)
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if strings.Contains(link, "zoneinfo/") {
			parts := strings.Split(link, "zoneinfo/")
			if len(parts) > 1 {
				return parts[1]
			}
		}
	}

	// Method 3: Try to read from /etc/timezone (Linux)
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(string(data))
	}

	return "Asia/Kolkata" // Fallback to a default timezone
}

func getCacheFilePath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "anime_cache.json" // fallback to current directory
	}
	return filepath.Join(homeDir, ".cache", "baka", "anime_schedule.json")
}

func saveTimetableCache(timetables []AnimeTimetable) error {
	cacheFile := getCacheFilePath()

	// Create cache directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(timetables, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(cacheFile, data, 0644)
}

func loadTimetableCache() ([]AnimeTimetable, error) {
	cacheFile := getCacheFilePath()

	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, err
	}

	var timetables []AnimeTimetable
	if err := json.Unmarshal(data, &timetables); err != nil {
		return nil, err
	}

	return timetables, nil
}

func isCacheValid() bool {
	cacheFile := getCacheFilePath()

	info, err := os.Stat(cacheFile)
	if err != nil {
		return false
	}

	// Cache is valid if it's less than 1 hour old
	return time.Since(info.ModTime()) < time.Hour
}

func fetchTimetableCmd() tea.Msg {
	// Try to load from cache first
	if isCacheValid() {
		if cachedTimetables, err := loadTimetableCache(); err == nil {
			return fetchTimetableMsg(cachedTimetables)
		}
	}

	apiToken, success := getEnvVariable("ANIMESCHEDULE_TOKEN")
	if !success {
		return errMsg(fmt.Errorf("ANIMESCHEDULE_TOKEN environment variable not set"))
	}

	// Get system timezone in proper format
	timezone := getSystemTimezone()

	options := map[string]any{
		"airType": "sub",
		"tz":      timezone,
	}

	timetable, err := fetchTimetables(apiToken, options)
	if err != nil {
		return errMsg(err)
	}

	// Save to cache
	if err := saveTimetableCache(timetable); err != nil {
		// Don't fail the whole operation if cache save fails
		fmt.Printf("Warning: Failed to save cache: %v\n", err)
	}

	return fetchTimetableMsg(timetable)
}

func getEnvVariable(key string) (string, bool) {
	// First try to get from actual environment variables
	if value, exists := os.LookupEnv(key); exists {
		return value, true
	}

	// Then try to read from .env file
	file, err := os.Open(".env")
	if err != nil {
		return "", false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"="), true
		}
	}

	return "", false
}

func (m weeklyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "esc":
			// Only quit if not in filter mode
			if m.state == stateWeekly && m.list.FilterState() != list.Filtering {
				return m, tea.Quit
			}
		}

	case fetchTimetableMsg:
		// Data loaded successfully, switch to weekly view
		m.state = stateWeekly
		m.keys = newListKeyMap()
		m.delegateKeys = newDelegateKeyMap()

		// Populate allAnime slice with anime
		for _, anime := range msg {
			m.allAnime = append(m.allAnime, animeItem{anime: anime})
		}

		// Initialize the list
		m.list = list.New([]list.Item{}, newItemDelegate(m.delegateKeys), m.width-4, m.height-6)
		// Format initial day name with consistent width
		dayName := fmt.Sprintf("%-9s", m.focusedDay.String())
		m.list.Title = dayName
		m.list.Styles.Title = titleStyle
		m.list.SetShowHelp(false)
		m.list.SetShowStatusBar(false)

		// Set filter input width to match anime list width
		m.list.Styles.FilterPrompt = lipgloss.NewStyle().
			Width(50).
			MaxWidth(50).
			Inline(true)
		m.list.Styles.FilterCursor = lipgloss.NewStyle().
			Width(50).
			MaxWidth(50).
			Inline(true)

		m = m.updateListForDay()
		return m, nil

	case errMsg:
		m.err = msg
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.state == stateWeekly {
			// Update the list model to use full terminal size
			m.list.SetSize(msg.Width-4, msg.Height-6) // Account for title and help text
		}
		return m, nil
	}

	switch m.state {
	case stateLoading:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case stateWeekly:
		// Handle navigation between days first
		if msg, ok := msg.(tea.KeyMsg); ok {
			switch msg.String() {
			case "left", "h":
				// Don't navigate if filtering is active
				if m.list.FilterState() != list.Filtering {
					m.focusedDay = m.getPreviousDay()
					m = m.updateListForDay()
					return m, nil
				}
			case "right", "l":
				// Don't navigate if filtering is active
				if m.list.FilterState() != list.Filtering {
					m.focusedDay = m.getNextDay()
					m = m.updateListForDay()
					return m, nil
				}
			case "/":
				// When starting to filter, load all anime
				m = m.loadAllAnimeForFiltering()
				// Let the list handle the filter key
			}
		}

		// Update the list model
		var cmd tea.Cmd
		newListModel, cmd := m.list.Update(msg)

		// Check if filter state changed
		if newListModel.FilterState() != m.list.FilterState() || newListModel.FilterValue() != m.list.FilterValue() {
			m.list = newListModel
			m = m.updateListBasedOnFilterState()
			return m, cmd
		}

		m.list = newListModel
		return m, cmd
	}

	return m, nil
}

func (m weeklyModel) View() string {
	switch m.state {
	case stateLoading:
		if m.err != nil {
			errorText := fmt.Sprintf("Error: %v\n\nPress q to quit", m.err)
			return lipgloss.NewStyle().
				Align(lipgloss.Center, lipgloss.Center).
				Width(m.width).
				Height(m.height).
				Render(errorText)
		}
		loadingText := fmt.Sprintf("%s Fetching anime timetable...\n\nPress q to quit", m.spinner.View())
		return lipgloss.NewStyle().
			Align(lipgloss.Center, lipgloss.Center).
			Width(m.width).
			Height(m.height).
			Render(loadingText)

	case stateWeekly:
		// Center the list view
		centeredList := lipgloss.NewStyle().
			Align(lipgloss.Center).
			Width(m.width).
			Render(m.list.View())

		helpText := lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Align(lipgloss.Center).
			Width(m.width).
			Render("← → / h l: navigate days • ↑↓: select anime • enter: choose • x: delete • q: quit")

		return centeredList + "\n" + helpText
	}

	return ""
}

func (m weeklyModel) filterAnimeByDay(day time.Weekday) []list.Item {
	var items []list.Item
	for _, anime := range m.allAnime {
		if anime.anime.EpisodeDate.Weekday() == day {
			items = append(items, anime)
		}
	}
	return items
}

func (m weeklyModel) updateListForDay() weeklyModel {
	var items []list.Item

	// If filtering is active, show all anime across all days
	if m.list.FilterState() == list.Filtering || m.list.FilterValue() != "" {
		// Show all anime when searching
		for _, anime := range m.allAnime {
			items = append(items, anime)
		}
	} else {
		// Show only current day's anime when not searching
		items = m.filterAnimeByDay(m.focusedDay)
	}

	// Store current filter state before recreating list
	currentFilter := m.list.FilterValue()
	isFiltering := m.list.FilterState() == list.Filtering

	// Recreate the list with new delegate
	m.list = list.New(items, newItemDelegate(m.delegateKeys), m.width-4, m.height-6)

	// Restore filter state if it was active
	if currentFilter != "" {
		m.list.SetFilteringEnabled(true)
		// Trigger filtering mode and restore filter value
		if isFiltering {
			// This will put the list back into filtering mode
			m.list.SetShowFilter(true)
		}
	}

	// Format day name with consistent width
	dayName := fmt.Sprintf("%-9s", m.focusedDay.String())
	if m.list.FilterState() == list.Filtering || m.list.FilterValue() != "" {
		dayName = "All Days"
	}
	m.list.Title = dayName
	m.list.Styles.Title = titleStyle
	m.list.SetShowHelp(false)
	m.list.SetShowStatusBar(false)

	return m
}

func (m weeklyModel) getPreviousDay() time.Weekday {
	days := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	for i, day := range days {
		if day == m.focusedDay {
			if i == 0 {
				return days[len(days)-1] // wrap to Sunday
			}
			return days[i-1]
		}
	}
	return time.Monday
}

func (m weeklyModel) getNextDay() time.Weekday {
	days := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	for i, day := range days {
		if day == m.focusedDay {
			if i == len(days)-1 {
				return days[0] // wrap to Monday
			}
			return days[i+1]
		}
	}
	return time.Monday
}

func (m weeklyModel) loadAllAnimeForFiltering() weeklyModel {
	// Load all anime from all days when starting to filter
	var items []list.Item
	for _, anime := range m.allAnime {
		items = append(items, anime)
	}

	// Update the list with all anime and set title to "All Days"
	m.list.SetItems(items)
	m.list.Title = "All Days"
	m.list.SetFilteringEnabled(true)

	return m
}

func (m weeklyModel) updateListBasedOnFilterState() weeklyModel {
	if m.list.FilterState() == list.Filtering || m.list.FilterValue() != "" {
		// When filtering, show all anime from all days
		var items []list.Item
		for _, anime := range m.allAnime {
			items = append(items, anime)
		}
		m.list.SetItems(items)
		m.list.Title = "All Days"
	} else {
		// When not filtering, show only current day's anime
		items := m.filterAnimeByDay(m.focusedDay)
		m.list.SetItems(items)
		dayName := fmt.Sprintf("%-9s", m.focusedDay.String())
		m.list.Title = dayName
	}
	return m
}

type MediaType struct {
	Name  string `json:"name"`
	Route string `json:"route"`
}

type Streams struct {
	Crunchyroll string `json:"crunchyroll,omitempty"`
	Amazon      string `json:"amazon,omitempty"`
	Hidive      string `json:"hidive,omitempty"`
	Youtube     string `json:"youtube,omitempty"`
	Apple       string `json:"apple,omitempty"`
	Netflix     string `json:"netflix,omitempty"`
	Hulu        string `json:"hulu,omitempty"`
}

type AnimeTimetable struct {
	Title                   string      `json:"title"`
	Route                   string      `json:"route"`
	Romaji                  string      `json:"romaji,omitempty"`
	English                 string      `json:"english,omitempty"`
	Native                  string      `json:"native,omitempty"`
	DelayedText             string      `json:"delayedText,omitempty"`
	DelayedFrom             time.Time   `json:"delayedFrom"`
	DelayedUntil            time.Time   `json:"delayedUntil"`
	Status                  string      `json:"status"`
	EpisodeDate             time.Time   `json:"episodeDate"`
	EpisodeNumber           int         `json:"episodeNumber"`
	SubtractedEpisodeNumber int         `json:"subtractedEpisodeNumber,omitempty"`
	Episodes                int         `json:"episodes"`
	LengthMin               int         `json:"lengthMin"`
	Donghua                 bool        `json:"donghua"`
	AirType                 string      `json:"airType"`
	MediaTypes              []MediaType `json:"mediaTypes"`
	ImageVersionRoute       string      `json:"imageVersionRoute"`
	Streams                 Streams     `json:"streams"`
	AiringStatus            string      `json:"airingStatus"`
}

func fetchTimetables(apiToken string, options map[string]any) ([]AnimeTimetable, error) {
	baseUrl := "https://animeschedule.net/api/v3/timetables"

	url, err := url.Parse(baseUrl)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %v", err)
	}
	if airType, ok := options["airType"].(string); ok && airType != "" {
		url.Path += "/" + airType
	}

	queryParams := url.Query()
	if week, ok := options["week"].(int); ok && week > 0 {
		queryParams.Add("week", fmt.Sprintf("%d", week))
	}
	if year, ok := options["year"].(int); ok && year > 0 {
		queryParams.Add("year", fmt.Sprintf("%d", year))
	}
	if tz, ok := options["tz"].(string); ok && tz != "" {
		queryParams.Add("tz", tz)
	}
	url.RawQuery = queryParams.Encode()

	req, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		return nil, fmt.Errorf("API request failed: %s, response: %s", res.Status, string(body))
	}

	var result []AnimeTimetable
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode json response: %v", err)
	}

	return result, nil
}

func main() {
	p := tea.NewProgram(initialModel(""), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running program: %v", err)
		os.Exit(1)
	}
}
