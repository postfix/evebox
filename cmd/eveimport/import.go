/* Copyright (c) 2016 Jason Ish
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions
 * are met:
 *
 * 1. Redistributions of source code must retain the above copyright
 *    notice, this list of conditions and the following disclaimer.
 * 2. Redistributions in binary form must reproduce the above copyright
 *    notice, this list of conditions and the following disclaimer in the
 *    documentation and/or other materials provided with the distribution.
 *
 * THIS SOFTWARE IS PROVIDED ``AS IS'' AND ANY EXPRESS OR IMPLIED
 * WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
 * DISCLAIMED. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY DIRECT,
 * INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
 * (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
 * SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION)
 * HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT,
 * STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING
 * IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

package eveimport

import (
	"fmt"
	"github.com/jasonish/evebox/elasticsearch"
	"github.com/jasonish/evebox/evereader"
	flag "github.com/spf13/pflag"
	"os"
	"io"
	"time"
	"github.com/jasonish/evebox/log"
)

var flagset *flag.FlagSet

func usage() {
	usage := `Usage: evebox import [options] /path/to/eve.json

Options:
`
	fmt.Fprint(os.Stderr, usage)
	flagset.PrintDefaults()
}

func Main(args []string) {

	var elasticSearchUri string
	var oneshot bool
	var index string
	var verbose bool
	var end bool
	var batchSize uint64
	var useBookmark bool
	var bookmarkPath string

	flagset = flag.NewFlagSet("import", flag.ExitOnError)
	flagset.Usage = usage
	flagset.StringVarP(&elasticSearchUri, "elasticsearch", "e", "", "Elastic Search URL")
	flagset.BoolVar(&oneshot, "oneshot", false, "One shot mode (exit on EOF)")
	flagset.StringVar(&index, "index", "evebox", "Elastic Search index prefix")
	flagset.BoolVarP(&verbose, "verbose", "v", false, "Verbose output")
	flagset.BoolVar(&end, "end", false, "Start at end of file")
	flagset.Uint64Var(&batchSize, "batch-size", 1000, "Batch import size")
	flagset.BoolVar(&useBookmark, "bookmark", false, "Bookmark location")
	flagset.StringVar(&bookmarkPath, "bookmark-path", "", "Path to bookmark file")
	flagset.Parse(args[1:])

	if verbose {
		log.SetLevel(log.DEBUG)
	}

	if batchSize < 1 {
		log.Fatal("Batch size must be greater than 0")
	}

	if elasticSearchUri == "" {
		log.Error("error: --elasticsearch is a required parameter")
		usage()
		os.Exit(1)
	}

	if len(flagset.Args()) == 0 {
		log.Fatal("error: no input file provided")
	}

	if useBookmark && bookmarkPath == "" {
		bookmarkPath = fmt.Sprintf("%s.bookmark", flagset.Args()[0])
		log.Info("Using bookmark file %s", bookmarkPath)
	}

	es := elasticsearch.New(elasticSearchUri)
	response, err := es.Ping()
	if err != nil {
		log.Fatal("error: failed to ping Elastic Search:", err)
	}
	log.Info("Connected to Elastic Search v%s (cluster:%s; name: %s)",
		response.Version.Number, response.ClusterName, response.Name)

	// Check if the template exists.
	templateExists, err := es.CheckTemplate(index)
	if !templateExists {
		log.Info("Template %s does not exist, creating...", index)
		err = es.LoadTemplate(index)
		if err != nil {
			log.Fatal("Failed to create template:", err)
		}
	} else {
		log.Info("Template %s exists, will not create.", index)
	}

	inputFiles := flagset.Args()

	indexer := elasticsearch.NewIndexer(es)
	indexer.IndexPrefix = index

	reader, err := evereader.New(inputFiles[0])
	if err != nil {
		log.Fatal(err)
	}

	// Attempt to read the bookmark.
	if useBookmark {
		currentBookmark, err := evereader.ReadBookmark(bookmarkPath)
		if err != nil {
			log.Debug("Failed to read current bookmark: %v", err)
		}
		if currentBookmark == nil {
			currentBookmark = evereader.GetBookmark(reader)
		}

		// Attempt to write the bookmark so we know we can.
		err = evereader.WriteBookmark(bookmarkPath, currentBookmark)
		if err != nil {
			log.Fatalf("Bookmark location (%s) not writable", bookmarkPath)
		}

		// Check if bookmark is valid for the current file.
		if !evereader.BookmarkIsValid(currentBookmark, reader) {
			if end {
				log.Info("Stale bookmark file found, will start reading from end of file.")
				reader.SkipToEnd()
			} else {
				log.Info("Stale bookmark file found, will start reading from beginning of file.")
			}
		} else {
			log.Debug("Skipping to line %d", currentBookmark.Offset)
			err = reader.SkipTo(currentBookmark.Offset)
			if err != nil {
				log.Error("Failed to jump bookmark location, will start at end of file: %s", err)
				err = reader.SkipToEnd()
				if err != nil {
					log.Error("Sigh, failed to skip to end of file, will just read...")
				}
			}
		}
	} else if end {
		log.Info("Jumping to end of file.")
		err := reader.SkipToEnd()
		if err != nil {
			log.Fatal(err)
		}
	}

	count := uint64(0)
	startTime := time.Now()

	go func() {
		err := indexer.Run()
		if err != nil {
			log.Fatal("Elastic Search indexer connection unexpectedly closed:", err)
		} else {
			log.Debug("Indexer exited without issue.")
		}
	}()

	for {
		eof := false
		event, err := reader.Next()
		if err != nil {
			if err == io.EOF {
				eof = true
			} else {
				log.Fatal(err)
			}
		}

		if event != nil {
			indexer.IndexRawEvent(event)
			count++
		}

		if eof || (count > 0 && count % batchSize == 0) {
			var bookmark *evereader.Bookmark = nil

			if useBookmark {
				bookmark = evereader.GetBookmark(reader)
			}

			response, err := indexer.FlushConnection()
			if err != nil {
				log.Fatal(err)
			}
			if response != nil {
				log.Debug("Indexed %d events {errors=%v}", len(response.Items),
					response.Errors)
			}

			if bookmark != nil {
				evereader.WriteBookmark(bookmarkPath, bookmark)
			}
		}

		if eof {
			if oneshot {
				break;
			} else {
				time.Sleep(1 * time.Second)
			}
		}
	}

	totalTime := time.Since(startTime)

	if oneshot {
		log.Info("Indexed %d events: time=%.2fs; avg=%d/s", count, totalTime.Seconds(),
			uint64(float64(count) / totalTime.Seconds()))
	}
}
