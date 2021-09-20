# static-site

static-site was written because hugo had too steep a learning curve. It reads all the files from
`--in`, runs them through Go's `templates/html` parser, and writes them to `--out`. For advanced
usage like json parsing, read the source code `main.go` it is much shorter than hugo's docs.

```
$ go get github.com/mgbelisle/static-site
$ static-site --help
Builds a static site using the html/template package, with TemplateData provided.

Usage: static-site [OPTIONS]

OPTIONS:
  -addr string
        Address to serve output dir, if provided
  -data string
        Data dir (for json data) (default "data")
  -in string
        Input dir (default "src")
  -max-open int
        Max number of files to open at once (default 100)
  -out string
        Output dir (default "docs")
  -templates string
        String separated list of template files/dirs. The first one is the base template (required) (default "templates/base.html templates")
  -verbose
        Verbose output
```
