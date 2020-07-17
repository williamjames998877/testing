package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var usagePrefix = fmt.Sprintf(`Builds a static site using the html/template package, with TemplateData provided.

Usage: %s [OPTIONS]

OPTIONS:
`, os.Args[0])

var (
	inFlag        = flag.String("in", "src", "Input dir")
	outFlag       = flag.String("out", "docs", "Output dir")
	dataFlag      = flag.String("data", "data", "Data dir (for json data)")
	templatesFlag = flag.String("templates", "templates/base.html templates", "String separated list of template files/dirs. The first one is the base template (required)")
	verboseFlag   = flag.Bool("verbose", false, "Verbose output")
	addrFlag      = flag.String("addr", "", "Address to serve output dir, if provided")
	maxOpenFlag   = flag.Int("max-open", 100, "Max number of files to open at once")
)

type TemplateData struct {
	URL    func(string) (string, error)
	Active func(string) (bool, error)
}

var TemplateFuncs = template.FuncMap{
	"json": func(file string) (interface{}, error) {
		data, err := ioutil.ReadFile(filepath.Join(*dataFlag, file))
		if err != nil {
			return nil, err
		}
		var obj interface{}
		return obj, json.Unmarshal(data, &obj)
	},
	"sprintf": func(format string, a ...interface{}) string {
		return fmt.Sprintf(format, a...)
	},
	"uniq": func() string {
		b := make([]byte, 16)
		rand.Read(b)
		return fmt.Sprintf("%x", b)
	},
	"read": func(file string) (string, error) {
		data, err := ioutil.ReadFile(filepath.Join(*dataFlag, file))
		if err != nil {
			return "", err
		}
		return string(data), nil
	},
	"html": func(v string) template.HTML {
		return template.HTML(v)
	},
}

var (
	logPrefix       = os.Args[0] + ": "
	verboseLogger   = log.New(ioutil.Discard, logPrefix, log.LstdFlags)
	errLogger       = log.New(os.Stderr, logPrefix, log.LstdFlags)
	maxOpenInLimit  = make(chan struct{})
	maxOpenOutLimit = make(chan struct{})
)

func main() {
	// Flag setup
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usagePrefix)
		flag.PrintDefaults()
	}
	flag.Parse()

	// Logger setup
	if *verboseFlag {
		verboseLogger = log.New(os.Stdout, logPrefix, log.LstdFlags)
	}
	maxOpenInLimit = make(chan struct{}, *maxOpenFlag/2)
	maxOpenOutLimit = make(chan struct{}, *maxOpenFlag/2)

	// Build once
	build(func(err error) {
		errLogger.Panic(err)
	})

	wg := sync.WaitGroup{}
	if *addrFlag != "" {
		// Serve at addr if provided
		wg.Add(1)
		go func() {
			defer wg.Add(-1)
			verboseLogger.Printf("Serving %s on %s", *outFlag, *addrFlag)
			if err := http.ListenAndServe(*addrFlag, http.FileServer(http.Dir(*outFlag))); err != nil {
				errLogger.Panic(err)
			}
		}()

		// Listen for changes
		wg.Add(1)
		go func() {
			defer wg.Add(-1)
			prevModTime := time.Now()
			for {
				rebuild := false
				checkChange := func(path string, info os.FileInfo) {
					if info.ModTime().After(prevModTime) {
						verboseLogger.Printf("Change detected in %s", path)
						rebuild = true
						prevModTime = info.ModTime()
					}
				}
				for _, path := range append([]string{
					*inFlag,
					*dataFlag,
				}, strings.Fields(*templatesFlag)...) {
					info, err := os.Stat(path)
					if err != nil {
						errLogger.Print(err)
						break
					}
					if info.IsDir() {
						if err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
							if err != nil {
								return err
							}
							checkChange(path, info)
							return nil
						}); err != nil {
							errLogger.Print(err)
							break
						}
					} else {
						checkChange(path, info)
					}
				}
				if rebuild {
					build(func(err error) {
						errLogger.Print(err)
					})
				}
				time.Sleep(time.Second)
			}
		}()
	}

	wg.Wait()
}

func build(errLogFunc func(error)) {
	// Templates setup
	templatesFields := strings.Fields(*templatesFlag)
	if len(templatesFields) < 1 {
		errLogFunc(errors.New("--templates requires at least the base template"))
		return
	}
	tmpl, err := template.New(filepath.Base(templatesFields[0])).Funcs(TemplateFuncs).ParseFiles(templatesFields[0])
	if err != nil {
		errLogFunc(err)
		return
	}
	verboseLogger.Printf("Parsed base template: %s", templatesFields[0])
	for _, path := range templatesFields[1:] {
		info, err := os.Stat(path)
		if err != nil {
			errLogFunc(err)
			return
		}
		if info.IsDir() {
			if err := filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !info.IsDir() {
					tmpl, err = tmpl.ParseFiles(path)
					if err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				errLogFunc(err)
				return
			}
		} else {
			tmpl, err = tmpl.ParseFiles(path)
			if err != nil {
				errLogFunc(err)
				return
			}
		}
		verboseLogger.Printf("Parsed templates: %s", path)
	}

	// Render the files
	if err := os.RemoveAll(*outFlag); err != nil {
		errLogFunc(err)
		return
	}
	wg := sync.WaitGroup{}
	if err := filepath.Walk(*inFlag, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(*inFlag, path)
		if err != nil {
			return err
		}
		outPath := filepath.Join(*outFlag, relPath)
		if info.IsDir() {
			// Make the dir
			verboseLogger.Printf("Creating dir: %s", outPath)
			if err := os.Mkdir(outPath, info.Mode()); err != nil {
				return err
			}
		} else {
			// Otherwise execute the template or copy the file, whichever is appropriate.
			// Do them all in parallel
			wg.Add(1)
			go func(path string, outPath string, info os.FileInfo) {
				defer wg.Add(-1)
				maxOpenOutLimit <- struct{}{}
				outFile, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE, info.Mode())
				defer func() {
					if outFile != nil {
						outFile.Close()
					}
					<-maxOpenOutLimit
				}()
				if err != nil {
					errLogFunc(err)
					return
				}
				rootPath, err := filepath.Rel(filepath.Dir(path), *inFlag)
				if err != nil {
					errLogFunc(err)
					return
				}
				if tmpl != nil && filepath.Ext(path) == ".html" {
					verboseLogger.Printf("Executing template: %s", path)
					tmpl2, err := tmpl.Clone()
					if err != nil {
						errLogFunc(err)
						return
					}
					tmpl2, err = tmpl2.ParseFiles(path)
					if err != nil {
						errLogFunc(err)
						return
					}
					if err := tmpl2.Execute(outFile, &TemplateData{
						URL: func(url string) (string, error) {
							if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
								return url, nil
							}
							fromSlash := filepath.FromSlash(url)
							stat := fromSlash
							if filepath.IsAbs(stat) {
								stat = filepath.Join(*inFlag, stat)
							} else {
								return "", errors.New("Relative paths not supported yet") // TODO
							}
							if info, err := os.Stat(stat); err != nil {
								return "", err
							} else if info.IsDir() {
								if _, err := os.Stat(filepath.Join(stat, "index.html")); err != nil {
									return "", err
								}
							}
							return filepath.ToSlash(filepath.Join(rootPath, fromSlash)), nil
						},
						Active: func(url string) (bool, error) {
							if url == "/" {
								return relPath == "index.html", nil
							}
							if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
								return false, nil
							}
							fromSlash := filepath.FromSlash(url)
							if filepath.IsAbs(fromSlash) {
								return strings.HasPrefix(relPath, strings.TrimPrefix(fromSlash, string(filepath.Separator))), nil
							} else {
								return false, errors.New("Relative paths not supported yet") // TODO
							}
						},
					}); err != nil {
						errLogFunc(err)
						return
					}
				} else {
					verboseLogger.Printf("Copying file: %s", path)
					maxOpenInLimit <- struct{}{}
					inFile, err := os.Open(path)
					defer func() {
						if inFile != nil {
							inFile.Close()
						}
						<-maxOpenInLimit
					}()
					if err != nil {
						errLogFunc(err)
						return
					}
					if _, err := io.Copy(outFile, inFile); err != nil {
						errLogFunc(err)
						return
					}
				}
			}(path, outPath, info)
		}
		return nil
	}); err != nil {
		errLogFunc(err)
		return
	}
	wg.Wait()
}
