package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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
	return i.anime.Title
}

func (i animeItem) Description() string {
	return fmt.Sprintf("Episode %d • %s • %s",
		i.anime.EpisodeNumber,
		i.anime.EpisodeDate.Format("Jan 2, 15:04"),
		i.anime.AirType)
}

func (i animeItem) FilterValue() string {
	return i.anime.Title
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
	dailyLists   map[time.Weekday]list.Model
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

	return weeklyModel{
		state:      stateLoading,
		spinner:    s,
		dailyLists: make(map[time.Weekday]list.Model),
		focusedDay: time.Monday,
		width:      80, // Set default width
		height:     24, // Set default height
	}
}

func (m weeklyModel) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		fetchTimetableCmd,
		tea.EnterAltScreen,
	)
}

func fetchTimetableCmd() tea.Msg {
	apiToken, success := getEnvVariable("ANIMESCHEDULE_TOKEN")
	if !success {
		return errMsg(fmt.Errorf("ANIMESCHEDULE_TOKEN environment variable not set"))
	}

	options := map[string]any{
		"airType": "sub",
	}

	timetable, err := fetchTimetables(apiToken, options)
	if err != nil {
		return errMsg(err)
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
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}

	case fetchTimetableMsg:
		// Data loaded successfully, switch to weekly view
		m.state = stateWeekly
		m.keys = newListKeyMap()
		m.delegateKeys = newDelegateKeyMap()

		// Initialize all weekday lists
		for day := time.Sunday; day <= time.Saturday; day++ {
			dailyList := list.New([]list.Item{}, newItemDelegate(m.delegateKeys), m.width-4, m.height-6)
			dailyList.Title = day.String()
			dailyList.Styles.Title = titleStyle
			dailyList.SetShowHelp(false)
			dailyList.SetShowStatusBar(false)
			m.dailyLists[day] = dailyList
		}

		// Populate daily lists with anime
		for _, anime := range msg {
			weekday := anime.EpisodeDate.Weekday()
			if dailyList, exists := m.dailyLists[weekday]; exists {
				dailyList.InsertItem(len(dailyList.Items()), animeItem{anime: anime})
				m.dailyLists[weekday] = dailyList
			}
		}
		return m, nil

	case errMsg:
		m.err = msg
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.state == stateWeekly {
			// Update all daily lists to use full terminal size
			for day := range m.dailyLists {
				dailyList := m.dailyLists[day]
				dailyList.SetSize(msg.Width-4, msg.Height-6) // Account for day indicator and help text
				m.dailyLists[day] = dailyList
			}
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
				m.focusedDay = m.getPreviousDay()
				return m, nil
			case "right", "l":
				m.focusedDay = m.getNextDay()
				return m, nil
			}
		}

		// Update the focused day's list
		if dailyList, exists := m.dailyLists[m.focusedDay]; exists {
			var cmd tea.Cmd
			dailyList, cmd = dailyList.Update(msg)
			m.dailyLists[m.focusedDay] = dailyList
			return m, cmd
		}
	}

	return m, nil
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

func (m weeklyModel) View() string {
	switch m.state {
	case stateLoading:
		if m.err != nil {
			return fmt.Sprintf("\n\n   Error: %v\n\n   Press q to quit", m.err)
		}
		return fmt.Sprintf("\n\n   %s Fetching anime timetable...\n\n   Press q to quit", m.spinner.View())

	case stateWeekly:
		// Show only the current focused day
		if dailyList, exists := m.dailyLists[m.focusedDay]; exists {
			// Show day navigation indicator
			days := []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
			currentIndex := 0
			for i, day := range days {
				if day == m.focusedDay {
					currentIndex = i
					break
				}
			}

			dayIndicator := fmt.Sprintf("%s (%d/7)", m.focusedDay.String(), currentIndex+1)
			dayIndicatorStyle := lipgloss.NewStyle().
				Foreground(lipgloss.Color("205")).
				Bold(true).
				Align(lipgloss.Center).
				Width(m.width)

			// Center the list view without border, using full terminal width
			centeredList := lipgloss.NewStyle().
				Align(lipgloss.Center).
				Width(m.width).
				Render(dailyList.View())

			helpText := lipgloss.NewStyle().
				Foreground(lipgloss.Color("241")).
				Align(lipgloss.Center).
				Width(m.width).
				Render("← → / h l: navigate days • ↑↓: select anime • enter: choose • x: delete • q: quit")

			return dayIndicatorStyle.Render(dayIndicator) + "\n" + centeredList + "\n" + helpText
		}
	}

	return ""
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
