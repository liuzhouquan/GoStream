package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func main() {
	dsn := flag.String("dsn", "data/gostream.db", "SQLite database path used by the API")
	videos := flag.Int("videos", 1000, "published videos to insert")
	flag.Parse()
	if *videos < 1 || *videos > 100000 {
		log.Fatal("videos must be 1-100000")
	}
	db, err := gorm.Open(sqlite.Open(*dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	if err := db.AutoMigrate(&platform.User{}, &platform.Video{}); err != nil {
		log.Fatal(err)
	}
	author := platform.User{Username: fmt.Sprintf("load-author-%d", time.Now().UnixNano()), PasswordHash: "disabled-load-fixture", Nickname: "Load fixture"}
	if err := db.Create(&author).Error; err != nil {
		log.Fatal(err)
	}
	batch := make([]platform.Video, 0, 200)
	now := time.Now().UTC()
	for i := 0; i < *videos; i++ {
		batch = append(batch, platform.Video{AuthorID: author.ID, Title: fmt.Sprintf("load video %06d", i), MediaURL: "/media/load-fixture.mp4", Status: "published", CreatedAt: now.Add(-time.Duration(i) * time.Second)})
		if len(batch) == cap(batch) || i == *videos-1 {
			if err := db.Create(&batch).Error; err != nil {
				log.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	fmt.Printf("inserted %d synthetic published video rows for author %d into %s\n", *videos, author.ID, *dsn)
}
