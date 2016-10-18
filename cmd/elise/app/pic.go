package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/PuerkitoBio/goquery"
	log "github.com/Sirupsen/logrus"
	"github.com/ensonmj/elise/cmd/elise/assets"
	"github.com/ensonmj/elise/cmd/elise/util"
	"github.com/ensonmj/elise/htmlutil"
	"github.com/spf13/cobra"
	"github.com/yosssi/gohtml"
	"golang.org/x/net/html"
	"golang.org/x/sync/errgroup"
)

var (
	fWorkerNum int

	fTerm bool

	fCrawlerFile string
	fPubDir      string
	fFlushPub    bool
	fPicSplitCnt int

	fWidthMin  float64
	fHeightMin float64
	fRatioMin  float64 // width / height
	fRatioMax  float64
	fImgNumMin int

	fOTrim   bool
	fDevMode bool
)

func init() {
	flags := PicCmd.Flags()
	flags.IntVarP(&fWorkerNum, "parallel", "p", 10, "number of parallel parse worker")

	flags.BoolVarP(&fTerm, "term", "t", false, "read from stdin and output to stdout")

	flags.StringVarP(&fCrawlerFile, "crawlerFile", "f", "", "crawler result file")
	flags.StringVarP(&fPubDir, "pubDir", "P", "./pub", "public dir for store demonstration HTML file")
	flags.BoolVar(&fFlushPub, "flushPub", true, "flush public dir")
	flags.IntVarP(&fPicSplitCnt, "splitCount", "c", 100, "max line count for one output file")

	flags.Float64VarP(&fWidthMin, "widthMin", "W", 64.0, "image min width")
	flags.Float64VarP(&fHeightMin, "heightMin", "H", 64.0, "image min height")
	flags.Float64VarP(&fRatioMin, "ratioMin", "r", 0.35, "image width/height min value")
	flags.Float64VarP(&fRatioMax, "ratioMax", "R", 2.85, "image width/height max value")
	flags.IntVarP(&fImgNumMin, "imgNumMin", "n", 4, "image num min value which won't be filtered")

	flags.BoolVarP(&fOTrim, "outputTrim", "o", false, "print HTML after trimming")
	flags.BoolVarP(&fDevMode, "devMode", "D", false, "develop mode, using local assets")
}

type TextInfo struct {
	LineCnt *uint64
	Text    string // format: "'url'\t'HTML'"
}

type CrawlerResp struct {
	LandingPage string `json:"final_url"`
	// Title       string `json:"title"`
	HTML string `json:"html"`
}

type ImgItem struct {
	Src                             string
	Top, Left, Width, Height, Ratio float64
}

type ScoredGrp struct {
	Score    int
	ImgItems []ImgItem
}

type ScoredGrpSlice []ScoredGrp

func (sgs ScoredGrpSlice) Len() int {
	return len(sgs)
}

func (sgs ScoredGrpSlice) Swap(i, j int) {
	sgs[i], sgs[j] = sgs[j], sgs[i]
}

// descending order
func (sgs ScoredGrpSlice) Less(i, j int) bool {
	return sgs[j].Score < sgs[i].Score
}

type PicDesc struct {
	LP      string `json:"lp"`
	Title   string `json:"title"`
	SGSlice ScoredGrpSlice
}

var PicCmd = &cobra.Command{
	Use:   "pic",
	Short: "Use pictures to describe the webpage.",
	Long: `Check all pictures in the webpage, find the pictures which can best
represent the webpage according to web structure and something else.`,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat(fPubDir); os.IsNotExist(err) {
			if err = os.Mkdir(fPubDir, os.ModePerm); err != nil {
				return err
			}
		} else if fFlushPub {
			if err := util.FlushDir(fPubDir); err != nil {
				return err
			}
		}
		if !fTerm && fCrawlerFile == "" {
			return errors.New("Must specify 'crawlerFile' if 'term' is false")
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return parsePage()
	},
}

func parsePage() error {
	ctx, cancel := context.WithCancel(context.Background())
	picDescChan := make(chan *PicDesc, fWorkerNum)
	var writeEG errgroup.Group
	writeEG.Go(func() error {
		if fTerm {
			for picDesc := range picDescChan {
				b, err := json.Marshal(picDesc)
				if err != nil {
					log.WithError(err).Warn("Failed to marshal picDesc")
					continue
				}
				fmt.Printf("%s\t%s\n", picDesc.LP, string(b))
			}
			return nil
		}

		tmpl, err := initTmpl(fDevMode)
		if err != nil {
			log.WithError(err).Warn("Failed to init template")
			cancel()
		}

		base := filepath.Base(fCrawlerFile)
		noSuffix := strings.TrimSuffix(base, filepath.Ext(base))
		resPath := filepath.Join(fPubDir, noSuffix+".html")
		f, err := openHTML(resPath, tmpl)
		if err != nil {
			log.WithFields(log.Fields{
				"resPath": resPath,
				"err":     err,
			}).Warn("Failed to create output HTML file")
			cancel()
		}

		line := 0
		index := 0
		for picDesc := range picDescChan {
			err = produceHTML(f, tmpl, picDesc)
			if err != nil {
				log.WithFields(log.Fields{
					"resPath": resPath,
					"err":     err,
				}).Warn("Failed to produce HTML node")
				cancel()
			}

			line++
			if line >= fPicSplitCnt {
				closeHTML(f, tmpl)

				line = 0
				index++
				resPath = filepath.Join(fPubDir, noSuffix+"_"+strconv.Itoa(index)+".html")
				f, err = openHTML(resPath, tmpl)
				if err != nil {
					log.WithFields(log.Fields{
						"resPath": resPath,
						"err":     err,
					}).Warn("Failed to create output HTML file")
					cancel()
				}
			}
		}
		closeHTML(f, tmpl)
		return nil
	})

	textInfo := TextInfo{LineCnt: new(uint64)}
	textInfoChan := make(chan TextInfo, fWorkerNum)
	var workerEG errgroup.Group
	for i := 0; i < fWorkerNum; i++ {
		index := i
		workerEG.Go(func() error {
			for {
				select {
				case textInfo, ok := <-textInfoChan:
					log.WithFields(log.Fields{
						"index": index,
						"text":  textInfo.Text,
					}).Debug("Received text")
					if !ok {
						log.WithField("index", index).Debug("Worker exit")
						return nil
					}
					fields := strings.Split(textInfo.Text, "\t")
					if len(fields) != 2 {
						log.WithFields(log.Fields{
							"index": index,
							"text":  textInfo.Text,
						}).Warn("Text format is wrong")
						continue
					}
					var resp CrawlerResp
					if err := json.Unmarshal([]byte(fields[1]), &resp); err != nil {
						log.WithFields(log.Fields{
							"index": index,
							"text":  textInfo.Text,
							"err":   err,
						}).Warn("Failed to unmarshal")
						continue
					}
					doc, err := goquery.NewDocumentFromReader(strings.NewReader(resp.HTML))
					if err != nil {
						log.WithFields(log.Fields{
							"index": index,
							"err":   err,
						}).Warn("Failed to create document")
						continue
					}

					lp := resp.LandingPage
					picDesc, err := parseDoc(doc, lp)
					if err != nil {
						log.WithFields(log.Fields{
							"index": index,
							"err":   err,
						}).Debug("Failed to parse document")
						continue
					}

					atomic.AddUint64(textInfo.LineCnt, 1)
					picDescChan <- picDesc
				}
			}
		})
	}

	var f *os.File
	var err error
	if fTerm {
		f = os.Stdin
	} else {
		f, err = os.Open(fCrawlerFile)
		if err != nil {
			log.WithFields(log.Fields{
				"crawlerFile": fCrawlerFile,
				"err":         err,
			}).Fatal("Failed to open crawler result file")
			return err
		}
	}

	jobStarted := time.Now()
	sc := bufio.NewScanner(f)
	sc.Buffer([]byte{}, 2*1024*1024) // default 64k, change to 2M
	lineCount := 0
SCANLOOP:
	for sc.Scan() {
		select {
		case <-ctx.Done():
			log.WithError(ctx.Err()).Warn("Partial finished to extract img")
			break SCANLOOP
		default:
			textInfo.Text = sc.Text()
			textInfoChan <- textInfo
			lineCount++
		}
	}
	f.Close()
	close(textInfoChan)
	workerEG.Wait()

	close(picDescChan)
	writeEG.Wait()

	if err = sc.Err(); err != nil {
		log.WithFields(log.Fields{
			"file":         fCrawlerFile,
			"readLineCnt":  lineCount,
			"writeLineCnt": atomic.LoadUint64(textInfo.LineCnt),
			"elapsed":      time.Since(jobStarted),
			"err":          err,
		}).Warn("Failed to read line from file")
		return err
	}
	log.WithFields(log.Fields{
		"file":         fCrawlerFile,
		"readLineCnt":  lineCount,
		"writeLineCnt": atomic.LoadUint64(textInfo.LineCnt),
		"elapsed":      time.Since(jobStarted),
	}).Debug("Finished all the job")

	return nil
}

func parseDoc(doc *goquery.Document, url string) (*PicDesc, error) {
	title := doc.Find("title").Text()

	trimHTML(doc)
	if err := normalizeHTML(doc, url); err != nil {
		log.WithError(err).Debug("Failed to normalize HTML")
		return nil, err
	}
	trimBranch(doc)
	if fOTrim {
		str, _ := doc.Html()
		fmt.Printf("%s\037%s\036\n", url, gohtml.Format(str))
	}

	tree := extractTree(doc)
	if tree == nil {
		log.Debug("Empty HTML body")
		return nil, errors.New("empty HTML body")
	}

	picDesc := sortTree(tree)
	if picDesc == nil {
		log.Debug("Empty PicDesc")
		return nil, errors.New("Empty PicDesc")
	}

	picDesc.LP = url
	picDesc.Title = title
	log.WithField("picDesc", picDesc).Debug("Finished to parse one document")

	return picDesc, nil
}

// trim some node according selector
func trimHTML(doc *goquery.Document) {
	for _, selector := range []string{"head", "header", "footer", "aside",
		"script", "noscript", "style", "object", "iframe", "form", "input", "pre", "code",
		"nav", "a", "p", "span", "h1", "h2", "h3", "h4", "h5", "h6", "strong", "em"} {
		doc.Find(selector).Remove()
	}
}

func normalizeHTML(doc *goquery.Document, lpSrc string) error {
	num := 0
	doc.Find("img").Each(func(i int, sel *goquery.Selection) {
		for _, n := range sel.Nodes {
			var imgSrc string
			for _, attr := range n.Attr {
				if attr.Key == "data-src" || attr.Key == "data-original" {
					imgSrc = attr.Val
					break
				} else if attr.Key == "src" {
					imgSrc = attr.Val
				}
			}
			if imgSrc == "" {
				var buf bytes.Buffer
				html.Render(&buf, n)
				log.WithFields(log.Fields{
					"lpSrc": lpSrc,
					"node":  n,
					"HTML":  buf.String(),
				}).Debug("Can't find img src while normalizing")
				continue
			}

			lpURL, err := url.Parse(lpSrc)
			if err != nil {
				log.WithFields(log.Fields{
					"lpSrc": lpSrc,
					"err":   err,
				}).Debug("Failed to parse landing page url")
				continue
			}
			imgURL, err := url.Parse(imgSrc)
			if err != nil {
				log.WithFields(log.Fields{
					"imgSrc": imgSrc,
					"err":    err,
				}).Debug("Failed to parse img url")
				continue
			}

			absoluteImgSrc := lpURL.ResolveReference(imgURL).String()
			n.Attr = append(n.Attr, html.Attribute{Key: "prim-img", Val: absoluteImgSrc})
			log.WithFields(log.Fields{
				"absoluteImgSrc": absoluteImgSrc,
			}).Debug("Got img src")

			num++
		}
	})

	if num <= 0 {
		return errors.New("can't find any image node")
	}
	return nil
}

// trim branch which not include img node or unqualified img
func trimBranch(doc *goquery.Document) {
	// only one body node
	sel := doc.Find("body")
	if len(sel.Nodes) == 0 {
		return
	}
	htmlutil.TrimNode(sel.Nodes[0], func(n *html.Node) bool {
		// trim TextNode, CommentNode etc, which is not ElementNode
		if n.Type != html.ElementNode {
			return true
		}
		if n.FirstChild != nil {
			return false
		}
		// trim leaf node which is not img node
		if n.Data != "img" {
			return true
		}
		// don't trim bad img node before isomorphisim parse
		return false
	})
}

func extractTree(doc *goquery.Document) []*html.Node {
	sel := doc.Find("body")
	if len(sel.Nodes) == 0 {
		// empty HTML body
		return nil
	}

	var tree []*html.Node
	// only one body node
	for _, n := range htmlutil.ExtractIsomorphisms(sel.Nodes[0], leafEqual) {
		tree = append(tree, htmlutil.ExtractIsomorphicLeaf(n, leafEqual)...)
	}
	return tree
}

func leafEqual(c, n *html.Node) bool {
	if c.Data != n.Data {
		return false
	}
	return true
}

func sortTree(tree []*html.Node) *PicDesc {
	var sgs ScoredGrpSlice
	for _, n := range tree {
		sg := calcScore(n)
		if sg.Score < 1 {
			log.WithField("score", sg.Score).Debug("Score too low")
			continue
		}

		sgs = append(sgs, sg)
	}
	if sgs.Len() < 1 {
		return nil
	}
	sort.Sort(sgs)

	return &PicDesc{SGSlice: sgs}
}

func calcScore(n *html.Node) ScoredGrp {
	imgItems := extractImg(n)
	if len(imgItems) < fImgNumMin {
		log.WithField("num", len(imgItems)).Debug("Image num under threshold")
		return ScoredGrp{Score: 0}
	}
	return ScoredGrp{Score: len(imgItems), ImgItems: imgItems}
}

//          c--...--img
//         /
// a--..--b--c--...--img
//         \
//          c--...--img
func extractImg(n *html.Node) []ImgItem {
	for n.FirstChild.Data != "img" && n.FirstChild.NextSibling == nil {
		n = n.FirstChild
	}
	var imgItems []ImgItem
	for curr := n.FirstChild; curr != nil; curr = curr.NextSibling {
		for curr.Data != "img" {
			curr = curr.FirstChild
		}
		img := normalizeImg(curr)
		if filterImg(img) {
			continue
		}
		imgItems = append(imgItems, img)
	}

	// remove duplicates
	uniq := make(map[string]bool)
	length := len(imgItems)
	var totalWidth, totalHeight, totalRatio float64
	for i := 0; i < length; i++ {
		if _, ok := uniq[imgItems[i].Src]; !ok {
			totalWidth += imgItems[i].Width
			totalHeight += imgItems[i].Height
			totalRatio += imgItems[i].Ratio

			uniq[imgItems[i].Src] = true
			continue
		}
		imgItems = append(imgItems[:i], imgItems[i+1:]...)
		length--
		i--
	}
	if length <= 2 {
		return imgItems
	}

	// remove item which far away from average
	avgWidth := totalWidth / float64(length)
	avgHeight := totalHeight / float64(length)
	avgRatio := totalRatio / float64(length)
	for i := 0; i < length; i++ {
		img := imgItems[i]
		if !imgOnAverage(img, avgWidth, avgHeight, avgRatio) {
			imgItems = append(imgItems[:i], imgItems[i+1:]...)
			length--
			i--
		}
	}

	return imgItems
}

func normalizeImg(n *html.Node) ImgItem {
	var img ImgItem
	for _, attr := range n.Attr {
		switch attr.Key {
		case "prim-top", "prim_top":
			img.Top, _ = strconv.ParseFloat(attr.Val, 64)
		case "prim-left", "prim_left":
			img.Left, _ = strconv.ParseFloat(attr.Val, 64)
		case "prim-width", "prim_width":
			img.Width, _ = strconv.ParseFloat(attr.Val, 64)
		case "prim-height", "prim_height":
			img.Height, _ = strconv.ParseFloat(attr.Val, 64)
		case "prim-img":
			img.Src = attr.Val
		}
	}
	img.Ratio = img.Width / img.Height
	log.WithField("imgItem", img).Debug("Normalize image")
	return img
}

func filterImg(img ImgItem) bool {
	if filterImgbyRect(img) {
		return true
	}

	return filterImgbyExt(img)
}

func filterImgbyRect(img ImgItem) bool {
	width, height := img.Width, img.Height
	if width < fWidthMin || height < fHeightMin {
		log.WithFields(log.Fields{
			"width":  width,
			"height": height,
		}).Debug("Filtered by width or height")
		return true
	}
	ratio := img.Ratio
	if ratio < fRatioMin || ratio > fRatioMax {
		log.WithFields(log.Fields{
			"width":  width,
			"height": height,
			"ratio":  ratio,
		}).Debug("Filtered by width/height ratio")
		return true
	}

	return false
}

func filterImgbyExt(img ImgItem) bool {
	imgSrc := img.Src
	if imgSrc == "" {
		log.Warn("Can't find img src while filtering")
		return true
	}
	// some img has no extention
	ext := filepath.Ext(imgSrc)
	log.WithFields(log.Fields{
		"imgSrc": imgSrc,
		"ext":    ext,
	}).Debug("Get img extention")
	if ext == ".gif" {
		return true
	}

	return false
}

func imgOnAverage(img ImgItem, avgWidth, avgHeight, avgRatio float64) bool {
	ratio := img.Ratio
	if ratio == avgRatio {
		return true
	} else if math.Abs(ratio-avgRatio)/avgRatio > 0.1 {
		return false
	}

	width := img.Width
	height := img.Height
	if math.Abs(width-avgWidth)/avgWidth > 0.1 || math.Abs(height-avgHeight)/avgHeight > 0.1 {
		return false
	}

	return true
}

func initTmpl(devMode bool) (*template.Template, error) {
	// create output HTML file
	tmplStr, err := assets.FSString(devMode, "/assets/templates/layout.gohtml")
	if err != nil {
		return nil, err
	}
	return template.New("tmpl").Parse(tmplStr)
}

func openHTML(filename string, tmpl *template.Template) (f *os.File, err error) {
	f, err = os.Create(filename)
	if err != nil {
		return
	}

	err = tmpl.ExecuteTemplate(f, "header", nil)
	return
}

func produceHTML(f *os.File, tmpl *template.Template, picDesc *PicDesc) error {
	return tmpl.ExecuteTemplate(f, "item", picDesc)
}

func closeHTML(f *os.File, tmpl *template.Template) error {
	err := tmpl.ExecuteTemplate(f, "footer", nil)
	f.Close()
	return err
}
