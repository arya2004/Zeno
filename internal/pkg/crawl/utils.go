package crawl

import (
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/internetarchive/Zeno/internal/pkg/utils"
	"github.com/sirupsen/logrus"
)

var regexOutlinks *regexp.Regexp

func (c *Crawl) crawlSpeedLimiter() {
	maxConcurrentAssets := c.MaxConcurrentAssets
	var pauseTriggeredByCrawlSpeed = false

	for {
		// Pause if the waitgroup has exceeded 8 times the active workers.
		if c.Client.WaitGroup.Size() > int(*c.ActiveWorkers)*8 {
			c.Paused.Set(true)
			c.Queue.Paused.Set(true)
			pauseTriggeredByCrawlSpeed = true
			// Lower the number of concurrent assets we'll capture if the waitgroup exceeds 4 times the active workers (and the pause is caused by crawlSpeed)
		} else if c.Client.WaitGroup.Size() > int(*c.ActiveWorkers)*4 && pauseTriggeredByCrawlSpeed {
			c.MaxConcurrentAssets = 1
			c.Paused.Set(false)
			c.Queue.Paused.Set(false)
			// If the pause was triggered by crawlSpeed and everything is fine, fully reset state.
		} else if pauseTriggeredByCrawlSpeed {
			c.MaxConcurrentAssets = maxConcurrentAssets
			c.Paused.Set(false)
			c.Queue.Paused.Set(false)
			pauseTriggeredByCrawlSpeed = false
		}

		time.Sleep(time.Second / 10)
	}
}

func (c *Crawl) handleCrawlPause() {
	for {
		spaceLeft := float64(utils.GetFreeDiskSpace(c.JobPath).Avail) / float64(GB)
		if spaceLeft <= float64(c.MinSpaceRequired) {
			logrus.Errorln(fmt.Sprintf("Not enough disk space: %d GB required, %f GB available. "+
				"Please free some space for the crawler to resume.", c.MinSpaceRequired, spaceLeft))
			c.Paused.Set(true)
			c.Queue.Paused.Set(true)
		} else {
			c.Paused.Set(false)
			c.Queue.Paused.Set(false)
		}

		time.Sleep(time.Second)
	}
}

func extractLinksFromText(source string) (links []*url.URL) {
	// Extract links and dedupe them
	rawLinks := utils.DedupeStrings(regexOutlinks.FindAllString(source, -1))

	// Validate links
	for _, link := range rawLinks {
		URL, err := url.Parse(link)
		if err != nil {
			continue
		}

		err = utils.ValidateURL(URL)
		if err != nil {
			continue
		}

		links = append(links, URL)
	}

	return links
}

// Re-implement host limitation
func (c *Crawl) shouldPause(host string) bool {
	activeHostCount := c.Frontier.GetActiveHostCount(host)
	if activeHostCount >= c.MaxConcurrentRequestsPerDomain {
		logrus.Warnf("Pausing crawl for host %s: active requests (%d) exceed limit (%d)", 
			host, activeHostCount, c.MaxConcurrentRequestsPerDomain)
		return true
	}
	return false
}

func isStatusCodeRedirect(statusCode int) bool {
	if statusCode == 300 || statusCode == 301 ||
		statusCode == 302 || statusCode == 307 ||
		statusCode == 308 {
		return true
	}
	return false
}
