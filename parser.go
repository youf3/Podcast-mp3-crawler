package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/akamensky/argparse"
	_ "github.com/mattn/go-sqlite3"
	"github.com/nandosousafr/podfeed"
	"github.com/tcolgate/mp3"
)

func main() {
	parser := argparse.NewParser("print", "Fetch Podcast url and cut mp3s")
	url := parser.String("u", "url", &argparse.Options{Required: true, Help: "URL for podcast rss"})
	start := parser.Int("s", "start", &argparse.Options{Help: "Time to cut at the start", Default: 0})
	end := parser.Int("e", "end", &argparse.Options{Help: "Time to cut at the end", Default: 0})
	numThreads := parser.Int("n", "threads", &argparse.Options{Help: "Number of threads to use", Default: 10})

	err := parser.Parse(os.Args)
	if err != nil {
		fmt.Print(parser.Usage(err))
		return
	}
	database, _ := sql.Open("sqlite3", "./podcasts.db")

	fmt.Println("Fetching Podcast")
	podcast, err := podfeed.Fetch(*url)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(podcast.Title + " Fetched")

	initializeDB(podcast, database)
	newItems := insertToPodcast(podcast, database)

	sem := make(chan int, *numThreads)

	for _, item := range newItems {
		sem <- 1

		go func(item *podfeed.Item, dname string, start, end int, database *sql.DB) {
			processItem(item, podcast.Title, start, end, database)
			<-sem
		}(item, podcast.Title, *start, *end, database)
	}
}

func insertToPodcast(podcast podfeed.Podcast, database *sql.DB) []*podfeed.Item {

	statement, _ := database.Prepare(`INSERT INTO podcast(Title, Subtitle, Description, url, Language, Author) 
	SELECT ?, ?, ?, ?, ?, ? 
	WHERE NOT EXISTS(SELECT 1 FROM podcast WHERE Title = ?)`)
	statement.Exec(podcast.Title, podcast.Subtitle, podcast.Description, podcast.Link, podcast.Language, podcast.Author,
		podcast.Title)

	statement, _ = database.Prepare(`INSERT INTO owner (podcastid, Name, Email)
	SELECT podcast.id, ?, ?
	FROM podcast
	WHERE podcast.Title = ?`)
	statement.Exec(podcast.Owner.Name, podcast.Owner.Email, podcast.Title)

	statement, _ = database.Prepare(`INSERT INTO category (podcastid, category)
	SELECT podcast.id, ?
	FROM podcast
	WHERE podcast.Title = ?`)
	statement.Exec(podcast.Category.Text, podcast.Title)

	statement, _ = database.Prepare(`INSERT INTO item (podcastid, Title, Link, Duration,
	Author, Summary, Subtitle, Description, Image, EnclosureUrl)
	SELECT podcast.id, ?, ?, ?, ?, ?, ?, ?, ?, ?
	FROM podcast
	WHERE podcast.Title = ?`)

	rows, _ := database.Query(`SELECT item.Title, item.Processed
	FROM item
	INNER JOIN podcast ON item.podcastid = podcast.id 
	AND podcast.Title = ?`, podcast.Title)

	if isExist, _ := exists(podcast.Title); !isExist {
		os.Mkdir(podcast.Title, 0700)
	}

	var isProcessed bool
	var title string
	var result int
	var item *podfeed.Item
	var newItems []*podfeed.Item
	rows.Next()
	rows.Scan(&title, &isProcessed)

	for i := len(podcast.Items) - 1; i >= 0; i-- {
		item = &podcast.Items[i]
		fmt.Println("Processing " + item.Title)
		result = strings.Compare(title, item.Title)
		fmt.Println("Compared " + strconv.Itoa(result))
		if result != 0 {
			fmt.Println("Inserting " + item.Title)
			statement.Exec(item.Title, item.Link, item.Duration, item.Author, item.Summary,
				item.Subtitle, item.Description, item.Image.Href, item.Enclosure.Url, podcast.Title)
			newItems = append(newItems, item)
		} else if !isProcessed {
			newItems = append(newItems, item)
			rows.Next()
			rows.Scan(&title, &isProcessed)
		} else {
			rows.Next()
			rows.Scan(&title, &isProcessed)
		}
	}

	return newItems

}

func processItem(item *podfeed.Item, dname string, start, end int, database *sql.DB) string {
	fmt.Println("Processing " + item.Title)
	url := item.Enclosure.Url
	filename := filepath.Join(dname + "/" + item.Title + ".mp3")
	processMP3(url, start, end, filename)

	statement, _ := database.Prepare(`UPDATE item
		SET Processed = 1
		WHERE Title = ?`)

	result, err := statement.Exec(item.Title)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		log.Fatal(err)
	}
	if rows != 1 {
		log.Fatalf("expected to affect 1 row, affected %d", rows)
	}

	return filename
}

func initializeDB(podcast podfeed.Podcast, database *sql.DB) {
	statement, _ := database.Prepare(`CREATE TABLE IF NOT EXISTS podcast (
		id INTEGER PRIMARY KEY, Title TEXT, Subtitle TEXT, Description TEXT, 
		url TEXT, Language TEXT, Author TEXT, UNIQUE(Title))`)
	statement.Exec()
	statement, _ = database.Prepare(`CREATE TABLE IF NOT EXISTS owner (
		id INTEGER PRIMARY KEY, podcastid INTEGER, Name TEXT, Email TEXT, 
		FOREIGN KEY(podcastid) REFERENCES podcast(id), UNIQUE(podcastid))`)
	statement.Exec()
	statement, _ = database.Prepare(`CREATE TABLE IF NOT EXISTS category (
		id INTEGER PRIMARY KEY, podcastid INTEGER, category TEXT, 
		FOREIGN KEY(podcastid) REFERENCES podcast(id), UNIQUE(podcastid))`)
	statement.Exec()

	statement, _ = database.Prepare(`CREATE TABLE IF NOT EXISTS item (
		id INTEGER PRIMARY KEY, podcastid INTEGER, Title TEXT, Link TEXT, 
		Duration TEXT, Author TEXT, Summary TEXT, Subtitle TEXT, 
		Description TEXT, Image TEXT, EnclosureUrl TEXT, 
		Processed boolean DEFAULT false,
		FOREIGN KEY(podcastid) REFERENCES podcast(id), UNIQUE(Title))`)
	statement.Exec()
}

func processMP3(url string, headSkip, tailSkip int, fn string) {
	// nanosec to millsec
	headSkip *= 1000 * 1000
	tailSkip *= 1000 * 1000

	resp, err := http.Get(url)
	if err != nil {
		fmt.Println("Cannot find file from url " + url)
	}
	defer resp.Body.Close()

	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	outFile, err := os.Create(fn)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer outFile.Close()

	if headSkip == 0 && tailSkip == 0 {
		outFile.Write(buf)
	} else {
		skipped := 0
		d := mp3.NewDecoder(bytes.NewReader(buf))
		var f mp3.Frame
		var duration int

		origDuration := getDuration(d)
		tailSkip = origDuration - tailSkip

		d = mp3.NewDecoder(bytes.NewReader(buf))
		duration = 0
		for {
			if err := d.Decode(&f, &skipped); err != nil {
				fmt.Println(err)
				return
			}
			duration = duration + int(f.Duration())

			buf, err := ioutil.ReadAll(f.Reader())
			if err != nil {
				fmt.Println(err)
				return
			}

			if duration > headSkip && duration < tailSkip {
				outFile.Write(buf)
			}
		}
	}
}

func getDuration(d *mp3.Decoder) int {
	var f mp3.Frame
	var duration int
	skipped := 0
	duration = 0
	for {
		if err := d.Decode(&f, &skipped); err != nil {
			fmt.Println(err)
			return duration
		}
		duration = duration + int(f.Duration())
	}
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}
