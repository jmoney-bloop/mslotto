package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

var startUrl string = "https://www.mslottery.com/gamestatus/active/"

type PrizeTier struct {
	Value          int
	OriginalCount  int
	RemainingCount int
}

type Game struct {
	Name                 string
	Price                int
	Odds                 float64 // overall odds (“1:4.50” → 4.50)
	LaunchDate           string
	GameNumber           int
	PrizeTiers           []PrizeTier
	TotalOriginalPrizes  int // sum of all OriginalCount
	TotalRemainingPrizes int // sum of all RemainingCount
	URL                  string
}

func GetHTML() []byte {
	resp, err := http.Get(startUrl)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}
	return data
}
func GetLinks() []string {
	var links []string
	z := html.NewTokenizer(bytes.NewReader(GetHTML()))

	inActiveSession := false
	divDepth := 0

	for {
		tt := z.Next()

		if tt == html.ErrorToken {
			return links
		}

		token := z.Token()

		// Detect start of ACTIVE GAMES main div
		if tt == html.StartTagToken && token.Data == "div" {
			for _, a := range token.Attr {
				if a.Key == "class" && a.Val == "col-lg-3 gamebox" {
					inActiveSession = true
					divDepth = 1
					break
				}
			}

			// If already inside session, count nested divs
			if inActiveSession && !(token.Data == "div" && divDepth == 1) {
				divDepth++
			}
		}

		// Detect end tags
		if tt == html.EndTagToken && token.Data == "div" {
			if inActiveSession {
				divDepth--
				if divDepth == 0 {
					inActiveSession = false
				}
			}
		}

		// Capture links inside the active session
		if inActiveSession && tt == html.StartTagToken && token.Data == "a" {
			for _, a := range token.Attr {
				if a.Key == "href" {
					links = append(links, a.Val)
				}
			}
		}
	}
}

func GamePage(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		log.Fatal(err)
	}
	return io.ReadAll(resp.Body)
}

func ExtractTables(htmlBytes []byte) [][][]string {
	z := html.NewTokenizer(bytes.NewReader(htmlBytes))

	var tables [][][]string
	var currentTable [][]string
	var currentRow []string

	inTable := false
	inRow := false
	inCell := false

	for {
		tt := z.Next()
		switch tt {

		case html.ErrorToken:
			return tables

		case html.StartTagToken:
			t := z.Token()
			switch t.Data {
			case "table":
				inTable = true
				currentTable = [][]string{}

			case "tr":
				if inTable {
					inRow = true
					currentRow = []string{}
				}

			case "td", "th":
				if inRow {
					inCell = true
				}
			}

		case html.EndTagToken:
			t := z.Token()
			switch t.Data {
			case "td", "th":
				inCell = false

			case "tr":
				if inRow {
					inRow = false
					currentTable = append(currentTable, currentRow)
				}

			case "table":
				if inTable {
					inTable = false
					tables = append(tables, currentTable)
				}
			}

		case html.TextToken:
			if inCell {
				txt := strings.TrimSpace(z.Token().Data)
				if txt != "" {
					currentRow = append(currentRow, txt)
				}
			}
		}
	}
}
func ParseGame(url string) [][][]string {
	htmlBytes, err := GamePage(url)
	if err != nil {
		fmt.Println("Error fetching game page:", url, err)
		return nil
	}

	return ExtractTables(htmlBytes)
}

func ParseMetaData(table [][]string) (price int, odds float64, launchDate string) {
	for _, row := range table {
		if len(row) < 2 {
			continue
		}
		key := strings.ToLower(row[0])
		val := row[1]

		switch {
		case strings.Contains(key, "ticket price"):
			price = parseDollar(val)
		case strings.Contains(key, "overall odds"):
			odds = parseOdds(val)
		case strings.Contains(key, "launch date"):
			launchDate = val
		}
	}
	return price, odds, launchDate
}

func ParsePrizes(table [][]string) []PrizeTier {
	var prizes []PrizeTier

	for _, row := range table[1:] { // Skip header row
		if len(row) < 3 {
			continue
		}

		if strings.Contains(strings.ToLower(row[0]), "2nd chance") {
			continue
		}

		value := parseDollar(row[0])
		orig := parseInt(row[1])
		remain := parseInt(row[2])

		prizes = append(prizes, PrizeTier{
			Value:          value,
			OriginalCount:  orig,
			RemainingCount: remain,
		})
	}
	return prizes
}

func parseDollar(s string) int {
	s = strings.ReplaceAll(s, "$", "")
	s = strings.ReplaceAll(s, ",", "")
	n, _ := strconv.Atoi(s)
	return n
}

func parseOdds(s string) float64 {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0.0
	}
	f, _ := strconv.ParseFloat(parts[1], 64)
	return f
}

func parseInt(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}

func (g *Game) OriginalTickets() int {
	return int(math.Round(g.Odds * float64(g.TotalOriginalPrizes)))
}
func (g *Game) RemainingTickets() int {
	return int(math.Round(g.Odds * float64(g.TotalRemainingPrizes)))
}
func (g *Game) EV() float64 {
	remainingTickets := g.RemainingTickets()
	if remainingTickets == 0 {
		return float64(g.Price) // no remaining tickets, you “lose” your ticket
	}

	var expectedWin float64
	for _, p := range g.PrizeTiers {
		if p.RemainingCount <= 0 || p.Value <= 0 {
			continue
		}
		prob := float64(p.RemainingCount) / float64(remainingTickets)
		expectedWin += prob * float64(p.Value)
	}

	return float64(g.Price) - expectedWin
}

func exctractGameName(url string) string {
	parts := strings.Split(strings.Trim(url, "/"), "/")
	if len(parts) > 1 {
		return strings.ReplaceAll(parts[len(parts)-1], "-", " ")
	}
	return url
}
func BuildGame(tables [][][]string, name string, url string) Game {
	meta := tables[0]
	prizeTables := tables[1]

	price, odds, launchdate := ParseMetaData(meta)
	prizeTiers := ParsePrizes(prizeTables)

	var totalOrg, totalRemain int
	for _, p := range prizeTiers {
		totalOrg += p.OriginalCount
		totalRemain += p.RemainingCount
	}
	game := Game{
		Name:                 name,
		Price:                price,
		Odds:                 odds,
		LaunchDate:           launchdate,
		PrizeTiers:           prizeTiers,
		TotalOriginalPrizes:  totalOrg,
		TotalRemainingPrizes: totalRemain,
		URL:                  url,
	}
	return game
}

func WriteCSV(games []Game, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	w := csv.NewWriter(file)
	defer w.Flush()

	w.Write([]string{"Name", "Price", "Odds", "Launch Date", "Original Winning Tickets", "Remaining Winning Tickets", "Estimated Original Tickets", "Estimated Remaining Tickets", "EV", "URL"})
	for _, g := range games {
		ev := g.EV()
		w.Write([]string{
			g.Name,
			strconv.Itoa(g.Price),
			fmt.Sprintf("1:%.2f", g.Odds),
			g.LaunchDate,
			strconv.Itoa(g.TotalOriginalPrizes),
			strconv.Itoa(g.TotalRemainingPrizes),
			strconv.Itoa(g.OriginalTickets()),
			strconv.Itoa(g.RemainingTickets()),
			fmt.Sprintf("%.2f", ev),
			g.URL,
		})
	}
	return nil
}

func main() {
	sem := make(chan struct{}, 75) // limit to 5 concurrent requests
	var games []Game
	var mu sync.Mutex

	for _, link := range GetLinks() {
		sem <- struct{}{}
		go func(l string) {
			defer func() { <-sem }()

			tables := ParseGame(l)
			name := exctractGameName(l)
			g := BuildGame(tables, name, l)

			mu.Lock()
			games = append(games, g)
			mu.Unlock()
		}(link)
	}
	for i := 0; i < cap(sem); i++ {
		sem <- struct{}{}
	}
	sort.Slice(games, func(i, j int) bool {
		return games[i].EV() > games[j].EV()
	})
	err := WriteCSV(games, "mslotto_games.csv")
	if err != nil {
		log.Fatal("Error writing CSV:", err)
	}
	fmt.Println("Data written to mslotto_games.csv")
}
