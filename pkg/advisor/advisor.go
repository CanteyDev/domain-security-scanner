package advisor

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/smtp"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/globalcyberalliance/domain-security-scanner/v3/pkg/cache"
	"github.com/spf13/cast"
)

var emailRegex = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+\\/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")

type (
	Advisor struct {
		consumerDomains      map[string]struct{}
		consumerDomainsMutex *sync.Mutex
		dialer               *net.Dialer
		tlsCacheHost         *cache.Cache[[]string]
		tlsCacheMail         *cache.Cache[[]string]
		checkTLS             bool
	}

	Advice struct {
		Domain []string `json:"domain,omitempty" yaml:"domain,omitempty" doc:"Domain advice." example:"Your domain looks good! No further action needed."`
		BIMI   []string `json:"bimi,omitempty" yaml:"bimi,omitempty" doc:"BIMI advice." example:"Your BIMI record looks good! No further action needed."`
		DKIM   []string `json:"dkim,omitempty" yaml:"dkim,omitempty" doc:"DKIM advice." example:"DKIM is setup for this email server. However, if you have other 3rd party systems, please send a test email to confirm DKIM is setup properly."`
		DMARC  []string `json:"dmarc,omitempty" yaml:"dmarc,omitempty" doc:"DMARC advice." example:"You are currently at the lowest level and receiving reports, which is a great starting point. Please make sure to review the reports, make the appropriate adjustments, and move to either quarantine or reject soon."`
		MX     []string `json:"mx,omitempty" yaml:"mx,omitempty" doc:"MX advice." example:"You have a multiple mail servers setup! No further action needed."`
		SPF    []string `json:"spf,omitempty" yaml:"spf,omitempty" doc:"SPF advice." example:"SPF seems to be setup correctly! No further action needed."`
	}

	// dmarc represents the structure of a DMARC record.
	dmarc struct {
		Version                    string
		Policy                     string
		SubdomainPolicy            string
		Percentage                 int
		AggregateReportDestination []string
		ForensicReportDestination  []string
		FailureOptions             string
		ASPF                       string
		ADKIM                      string
		ReportInterval             int
		Advice                     []string
	}
)

func NewAdvisor(timeout time.Duration, cacheLifetime time.Duration, checkTLS bool) *Advisor {
	advisor := Advisor{
		checkTLS:             checkTLS,
		consumerDomains:      make(map[string]struct{}),
		consumerDomainsMutex: &sync.Mutex{},
		dialer:               &net.Dialer{Timeout: timeout},
		tlsCacheHost:         cache.New[[]string](cacheLifetime),
		tlsCacheMail:         cache.New[[]string](cacheLifetime),
	}

	for _, domain := range consumerDomainList {
		advisor.consumerDomains[domain] = struct{}{}
	}

	return &advisor
}

func (a *Advisor) CheckAll(domain, bimi, dkim, dmarc string, mx []string, spf string) *Advice {
	advice := &Advice{}
	var wg sync.WaitGroup

	wg.Add(6)
	go func() {
		advice.Domain = a.CheckDomain(domain)
		wg.Done()
	}()

	go func() {
		advice.BIMI = a.CheckBIMI(bimi)
		wg.Done()
	}()

	go func() {
		advice.DKIM = a.CheckDKIM(dkim)
		wg.Done()
	}()

	go func() {
		advice.DMARC = a.CheckDMARC(dmarc)
		wg.Done()
	}()

	go func() {
		advice.MX = a.CheckMX(mx)
		wg.Done()
	}()

	go func() {
		advice.SPF = a.CheckSPF(spf)
		wg.Done()
	}()

	wg.Wait()

	return advice
}

func (a *Advisor) CheckBIMI(bimi string) (advice []string) {
	if len(bimi) == 0 {
		return []string{"We couldn't detect any active BIMI record for your domain. Please visit https://dmarcguide.globalcyberalliance.org to fix this."}
	}

	if strings.Contains(bimi, ";") {
		bimiResult := strings.Split(bimi, ";")
		var svgFound, vmcFound bool

		for index, tag := range bimiResult {
			tag = strings.TrimSpace(tag)

			if index == 0 && !strings.Contains(tag, "v=BIMI1") {
				advice = append(advice, "The beginning of your BIMI record should be v=BIMI1 with specific capitalization.")
			}

			if strings.Contains(tag, "l=") {
				svgFound = true
				tagValue := strings.TrimPrefix(tag, "l=")

				// download SVG logo
				response, err := http.Head(tagValue)
				if err != nil || response == nil {
					advice = append(advice, "Your SVG logo could not be downloaded.")
					continue
				}
				defer response.Body.Close()

				if response.StatusCode != http.StatusOK {
					advice = append(advice, "Your SVG logo could not be downloaded.")
					continue
				}

				if response.ContentLength > int64(32*1024) {
					advice = append(advice, "Your SVG logo exceeds the maximum of 32KB.")
				}
			}

			if strings.Contains(tag, "a=") {
				vmcFound = true
				tagValue := strings.TrimPrefix(tag, "a=")

				// download VMC cert
				response, err := http.Head(tagValue)
				if err != nil || response == nil {
					advice = append(advice, "Your VMC certificate could not be downloaded.")
					continue
				}
				defer response.Body.Close()

				if response.StatusCode != http.StatusOK {
					advice = append(advice, "Your VMC certificate could not be downloaded.")
					continue
				}
			}
		}

		if !svgFound {
			advice = append(advice, "Your BIMI record is missing the SVG logo URL.")
		}

		if !vmcFound {
			advice = append(advice, "Your BIMI record is missing the VMC cert URL.")
		}
	} else {
		advice = append(advice, "Your BIMI record appears to be malformed as no semicolons seem to be present.")
	}

	if len(advice) == 0 {
		return []string{"Your BIMI record looks good! No further action needed."}
	}

	// prepend a message detailing that the BIMI record has some issues
	advice = append([]string{"Your BIMI record has some issues:"}, advice...)

	return advice
}

func (a *Advisor) CheckDKIM(dkim string) (advice []string) {
	if dkim == "" {
		return []string{"We couldn't detect any active DKIM record for your domain. Due to how DKIM works, we only lookup common/known DKIM selectors (such as x, selector1, google). Visit https://dmarcguide.globalcyberalliance.org for more info on how to configure DKIM for your domain."}
	}

	if strings.Contains(dkim, ";") {
		dkimResult := strings.Split(dkim, ";")

		for index, tag := range dkimResult {
			tag = strings.TrimSpace(tag)

			switch index {
			case 0:
				if !strings.Contains(tag, "v=DKIM1") {
					advice = append(advice, "The beginning of your DKIM record should be v=DKIM1 with specific capitalization.")
				}
			case 1:
				if !strings.Contains(tag, "k=rsa") && !strings.Contains(tag, "a=rsa-sha256") {
					advice = append(advice, "The second tag in your DKIM record must be k=rsa or a=rsa=sha256.")
				}
			case 2:
				if !strings.Contains(tag, "p=") {
					advice = append(advice, "The third tag in your DKIM record must be p=YOUR_KEY.")
				}
			}
		}
	} else {
		advice = append(advice, "Your DKIM record appears to be malformed as no semicolons seem to be present.")
	}

	if len(advice) == 0 {
		return []string{"DKIM is setup for this email server. However, if you have other 3rd party systems, please send a test email to confirm DKIM is setup properly."}
	}

	return advice
}

func (a *Advisor) CheckDMARC(record string) (advice []string) {
	if record == "" {
		return []string{"You do not have DMARC setup!"}
	}

	if !strings.Contains(record, ";") {
		return []string{"Your DMARC record appears to be malformed as no semicolons seem to be present."}
	}

	dmarcRecord := dmarc{}
	parts := strings.Split(record, ";")
	ruaExists := strings.Contains(record, "rua=")

	for index, part := range parts {
		keyValue := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(keyValue) != 2 {
			continue
		}

		key := keyValue[0]
		value := keyValue[1]

		switch key {
		case "v":
			if index != 0 || value != "DMARC1" {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "The beginning of your DMARC record should be v=DMARC1 with specific capitalization.")
			}

			dmarcRecord.Version = value
		case "p":
			if index != 1 {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "The second tag in your DMARC record must be p=none/p=quarantine/p=reject.")
			}

			dmarcRecord.Policy = value

			switch dmarcRecord.Policy {
			case "quarantine":
				if ruaExists {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "You are currently at the second level and receiving reports. Please make sure to review the reports, make the appropriate adjustments, and move to reject soon.")
				} else {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "You are currently at the second level. However, you must receive reports in order to determine if DKIM/DMARC/SPF are functioning correctly and move to the highest level (reject). Please add the ‘rua’ tag to your DMARC policy.")
				}
			case "none":
				if ruaExists {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "You are currently at the lowest level and receiving reports, which is a great starting point. Please make sure to review the reports, make the appropriate adjustments, and move to either quarantine or reject soon.")
				} else {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "You are currently at the lowest level, which is a great starting point. However, you must receive reports in order to determine if DKIM/DMARC/SPF are functioning correctly. Please add the ‘rua’ tag to your DMARC policy.")
				}
			case "reject":
				if ruaExists {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "You are at the highest level! Please make sure to continue reviewing the reports and make the appropriate adjustments, if needed.")
				} else {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "You are at the highest level! However, we do recommend keeping reports enabled (via the rua tag) in case any issues may arise and you can review reports to see if DMARC is the cause.")
				}
			default:
				dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid DMARC policy specified, the record must be p=none/p=quarantine/p=reject.")
			}
		case "sp":
			dmarcRecord.SubdomainPolicy = value

			if dmarcRecord.SubdomainPolicy != "none" && dmarcRecord.SubdomainPolicy != "quarantine" && dmarcRecord.SubdomainPolicy != "reject" {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid subdomain policy specified, the record must be sp=none/sp=quarantine/sp=reject.")
			}
		case "pct":
			pct, err := strconv.Atoi(value)
			if err != nil || pct < 0 || pct > 100 {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid report percentage specified, it must be between 0 and 100.")
			}

			dmarcRecord.Percentage = pct
		case "rua":
			dmarcRecord.AggregateReportDestination = strings.Split(value, ",")
			for _, destination := range dmarcRecord.AggregateReportDestination {
				if !strings.HasPrefix(destination, "mailto:") {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid aggregate report destination specified, it should begin with mailto:.")
				}

				if !validateEmail(strings.TrimPrefix(destination, "mailto:")) {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid aggregate report destination specified, it should be a valid email address.")
				}
			}
		case "ruf":
			dmarcRecord.ForensicReportDestination = strings.Split(value, ",")
			for _, destination := range dmarcRecord.ForensicReportDestination {
				if !strings.HasPrefix(destination, "mailto:") {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid forensic report destination specified, it should begin with mailto:.")
					continue
				}

				if !validateEmail(strings.TrimPrefix(destination, "mailto:")) {
					dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid forensic report destination specified, it should be a valid email address.")
				}
			}
		case "fo":
			dmarcRecord.FailureOptions = value
			if dmarcRecord.FailureOptions != "0" && dmarcRecord.FailureOptions != "1" && dmarcRecord.FailureOptions != "d" && dmarcRecord.FailureOptions != "s" {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid failure options specified, the record must be fo=0/fo=1/fo=d/fo=s.")
			}
		case "aspf":
			dmarcRecord.ASPF = value
		case "adkim":
			dmarcRecord.ADKIM = value
		case "ri":
			ri, err := strconv.Atoi(value)
			if err != nil {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid report interval specified, it must be a positive integer.")
			}

			if ri < 0 {
				dmarcRecord.Advice = append(dmarcRecord.Advice, "Invalid report interval specified, it must be a positive value.")
			}

			dmarcRecord.ReportInterval = ri
		}
	}

	if len(dmarcRecord.AggregateReportDestination) == 0 {
		dmarcRecord.Advice = append(dmarcRecord.Advice, "Consider specifying a 'rua' tag for aggregate reporting.")
	}

	if dmarcRecord.FailureOptions == "" {
		dmarcRecord.Advice = append(dmarcRecord.Advice, "Consider specifying an 'fo' tag to define the condition for generating failure reports. Default is '0' (report if both SPF and DKIM fail).")
	}

	if len(dmarcRecord.ForensicReportDestination) == 0 {
		dmarcRecord.Advice = append(dmarcRecord.Advice, "Consider specifying a 'ruf' tag for forensic reporting.")
	}

	if dmarcRecord.SubdomainPolicy == "" {
		dmarcRecord.Advice = append(dmarcRecord.Advice, "Subdomain policy isn't specified, they'll default to the main policy instead.")
	}

	return dmarcRecord.Advice
}

func (a *Advisor) CheckDomain(domain string) (advice []string) {
	a.consumerDomainsMutex.Lock()
	if _, ok := a.consumerDomains[domain]; ok {
		a.consumerDomainsMutex.Unlock()
		return []string{"Consumer based accounts (i.e gmail.com, yahoo.com, etc) are controlled by the vendor. They are responsible for setting DKIM, SPF and DMARC capabilities on their domains."}
	}
	a.consumerDomainsMutex.Unlock()

	if a.checkTLS {
		advice = append(advice, a.checkHostTLS(domain, 443)...)
	}

	if len(advice) == 0 {
		return []string{"Your domain looks good! No further action needed."}
	}

	return advice
}

func (a *Advisor) CheckMX(mx []string) (advice []string) {
	switch len(mx) {
	case 0:
		return []string{"You do not have any mail servers setup, so you cannot receive email at this domain."}
	case 1:
		advice = []string{"You have a single mail server setup, but it's recommended that you have at least two setup in case the first one fails."}
	default:
		advice = []string{"You have multiple mail servers setup, which is recommended."}
	}

	if a.checkTLS {
		for _, serverAddress := range mx {
			// prepend the hostname to the advice line
			mxAdvice := a.checkMailTls(serverAddress)
			for _, serverAdvice := range mxAdvice {
				// strip the trailing dot from DNS records
				advice = append(advice, serverAddress[:len(serverAddress)-1]+": "+serverAdvice)
			}
		}

		counter := 0
		for index, adviceItem := range advice {
			if len(mx) == 1 && index == 0 {
				continue
			}

			if strings.Contains(adviceItem, "no further action needed") {
				counter++
			}
		}

		if counter == len(advice) {
			return []string{"All of your domains are using TLS 1.3, no further action needed!"}
		}
	}

	if len(advice) == 0 {
		return []string{"You have a multiple mail servers setup! No further action needed."}
	}

	return advice
}

func (a *Advisor) CheckSPF(spf string) []string {
	if spf == "" {
		return []string{"We couldn't detect any active SPF record for your domain. Please visit https://dmarcguide.globalcyberalliance.org to fix this."}
	}

	if strings.Contains(spf, "all") {
		if strings.Contains(spf, "+all") {
			return []string{"Your SPF record contains the +all tag. It is strongly recommended that this be changed to either -all or ~all. The +all tag allows for any system regardless of SPF to send mail on the organization’s behalf."}
		}
	} else {
		return []string{"Your SPF record is missing the all tag. Please visit https://dmarcguide.globalcyberalliance.org to fix this."}
	}

	return []string{"SPF seems to be setup correctly! No further action needed."}
}

func (a *Advisor) checkHostTLS(hostname string, port int) (advice []string) {
	// strip the trailing dot from DNS records
	if string(hostname[len(hostname)-1]) == "." {
		hostname = hostname[:len(hostname)-1]
	}

	// check if the advice is already in the cache
	tlsAdvice := a.tlsCacheHost.Get(hostname)
	if tlsAdvice != nil {
		return *tlsAdvice
	}

	// set the advice in the cache after the function returns
	defer func() {
		a.tlsCacheHost.Set(hostname, &advice)
	}()

	if port == 0 {
		port = 443
	}

	conn, err := tls.DialWithDialer(a.dialer, "tcp", hostname+":"+cast.ToString(port), nil)
	if err != nil {
		if strings.Contains(err.Error(), "no such host") {
			// fill variable to satisfy deferred cache fill
			advice = []string{hostname + " could not be reached"}
			return advice
		}

		if strings.Contains(err.Error(), "certificate is not trusted") || strings.Contains(err.Error(), "failed to verify certificate") {
			advice = append(advice, "No valid certificate could be found.")

			conn, err = tls.DialWithDialer(a.dialer, "tcp", hostname+":"+cast.ToString(port), &tls.Config{InsecureSkipVerify: true})
			if err != nil {
				return advice
			}
		} else {
			return []string{"Failed to reach domain: " + err.Error()}
		}
	}
	defer conn.Close()

	advice = append(advice, checkTLSVersion(conn.ConnectionState().Version))

	return advice
}

func (a *Advisor) checkMailTls(hostname string) (advice []string) {
	// strip the trailing dot from DNS records
	if string(hostname[len(hostname)-1]) == "." {
		hostname = hostname[:len(hostname)-1]
	}

	// check if the advice is already in the cache
	tlsAdvice := a.tlsCacheMail.Get(hostname)
	if tlsAdvice != nil {
		return *tlsAdvice
	}

	// set the advice in the cache after the function returns
	defer func() {
		a.tlsCacheMail.Set(hostname, &advice)
	}()

	conn, err := a.dialer.Dial("tcp", hostname+":25")
	if err != nil {
		// fill variable to satisfy deferred cache fill
		if strings.Contains(err.Error(), "i/o timeout") {
			advice = []string{"Failed to reach domain before timeout"}
		} else {
			advice = []string{"Failed to reach domain"}
		}

		return advice
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, hostname)
	if err != nil {
		// fill variable to satisfy deferred cache fill
		advice = []string{"Failed to reach domain"}
		return advice
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         hostname,
	}

	if err = client.StartTLS(tlsConfig); err != nil {
		if strings.Contains(err.Error(), "certificate is not trusted") || strings.Contains(err.Error(), "failed to verify certificate") {
			advice = append(advice, "No valid certificate could be found.")

			// close the existing connection and create a new one as we can't reuse it in the same way as the checkHostTLS function
			if err = conn.Close(); err != nil {
				// fill variable to satisfy deferred cache fill
				advice = append(advice, "Failed to re-attempt connection without certificate verification")
				return advice
			}

			conn, err = a.dialer.Dial("tcp", hostname+"25")
			if err != nil {
				// fill variable to satisfy deferred cache fill
				advice = []string{"Failed to reach domain"}
				return advice
			}
			defer conn.Close()

			client, err = smtp.NewClient(conn, hostname)
			if err != nil {
				// fill variable to satisfy deferred cache fill
				advice = []string{"Failed to reach domain"}
				return advice
			}

			// retry with InsecureSkipVerify
			tlsConfig.InsecureSkipVerify = true
			if err = client.StartTLS(tlsConfig); err != nil {
				// fill variable to satisfy deferred cache fill
				advice = append(advice, "Failed to start TLS connection")
				return advice
			}
		} else {
			// fill variable to satisfy deferred cache fill
			advice = []string{"Failed to start TLS connection: " + err.Error()}
			return advice
		}
	}

	if state, ok := client.TLSConnectionState(); ok {
		advice = append(advice, checkTLSVersion(state.Version))
	}

	return advice
}

func checkTLSVersion(tlsVersion uint16) string {
	switch tlsVersion {
	case tls.VersionTLS10:
		return "Your domain is using TLS version 1.0 which is outdated, and should be upgraded to TLS 1.3."
	case tls.VersionTLS11:
		return "Your domain is using TLS version 1.1 which is outdated, and should be upgraded to TLS 1.3."
	case tls.VersionTLS12:
		return "Your domain is using TLS version 1.2, and should be upgraded to TLS 1.3."
	case tls.VersionTLS13:
		return "Your domain is using TLS 1.3, no further action needed!"
	}

	return "Your domain is using an unrecognized version of TLS, you should verify that it's using TLS 1.3 or above."
}

func validateEmail(email string) bool {
	if len(email) < 3 || len(email) > 254 {
		return false
	}
	return emailRegex.MatchString(email)
}
