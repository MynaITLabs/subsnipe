package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

// cnameResult stores the result of a CNAME query for a domain
type cnameResult struct {
	domain string
	cname  string
	err    error
}

var (
	found    		 	  []string
	notFound 		 	  []string
    outputFileName   	  string  	= "output.md"
	domain           	  string
	isExploitable         []string
	notExploitable        []string
	unknownExploitability []string
	fingerprintsFile      = filepath.Join("fingerprints", "can-i-take-over-xyz_fingerprints.json")
)

func main() {
	var rootCmd = &cobra.Command{
		Use:   "subsnipe [flags]",
		Short: "SubSnipe identifies potentially take-over-able subdomains",
		Example: `./subsnipe -d test.com`,
		Run:   run,
	}

	rootCmd.Flags().StringVarP(&domain, "domain", "d", "", "The domain to query for subdomains (required)")
	rootCmd.MarkFlagRequired("domain")

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error executing subSnipe: %s", err)
	}
}

func run(cmd *cobra.Command, args []string) {
	printIntro()
	
	// Check if the AppVersion was already set during compilation - otherwise manually get it from `./VERSION`
	CheckAppVersion()
	color.Yellow("Current version: %s\n\n", AppVersion)

	// check if a later version of this tool exists
	NotifyOfUpdates()

	if !checkDigAvailable() {
        log.Fatal("The 'dig' command is not available. Please ensure it is installed.")
		return
    }

	// if the app runs inside a docker container, the output has to be written into `./output/output.md`, because
	// we will mount the CWD inside the container into `./output/` 
	if os.Getenv("RUNNING_ENVIRONMENT") == "docker" {
		outputFileName = filepath.Join("output", outputFileName)
	}

	// Check if https://github.com/EdOverflow/can-i-take-over-xyz/blob/master/fingerprints.json differs from the local copy in
	// ./fingerprints/can-i-take-over-xyz_fingerprints.json (i.e., has been updated). If so, update our local copy
	if updated, err := updateFingerprints(); err != nil {
		log.Error("Error updating fingerprints:", err)
	} else if updated {
		log.Info("Fingerprints updated")
	} else {
		log.Info("Fingerprints are already up to date")
	}

	log.Info("Checking subdomains for: ", domain)
	queryCRTSH()
}

// Queries crt.sh for subdomains of the given domain and writes unique common names to a file
func queryCRTSH() {
	log.Info("Querying crt.sh for subdomains... (may take a moment)")

	url := fmt.Sprintf("https://crt.sh/?q=%s&output=json", domain)
	resp, err := http.Get(url)
	if err != nil {
		log.Error("Error querying crt.sh: ", err)
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error("Error reading response body: ", err)
		return
	}

	var data []map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		log.Error("Error unmarshaling JSON: ", err)
		return
	}

	uniqueCommonNames := extractUniqueCommonNames(data)

	subdomainsFilePath := "crt-subdomains.txt"
	if err := writeSubdomainsToFile(uniqueCommonNames, subdomainsFilePath); err != nil {
		log.Error("Error writing to file: ", err)
		return
	}

	log.Info("Unique common names have been extracted to ", subdomainsFilePath)
	checkCNAMEs(subdomainsFilePath)
}

// Reads subdomains from a file and queries for their CNAME records concurrently
func checkCNAMEs(subdomainsFilePath string) {
    log.Info("Querying CNAME records for subdomains...")

    subdomainsFile, err := os.Open(subdomainsFilePath)
    if err != nil {
        log.Error("Error opening file: ", err)
        return
    }
    defer subdomainsFile.Close()

    scanner := bufio.NewScanner(subdomainsFile)
    var wg sync.WaitGroup
    results := make(chan cnameResult, 100) // Buffer may be adjusted based on expected concurrency

    maxConcurrency := 20
    sem := make(chan struct{}, maxConcurrency) // Control concurrency with a semaphore

    fingerprints, err := loadFingerprints(fingerprintsFile)
    if err != nil {
        log.Fatalf("Error loading fingerprints: %v", err)
    }

    // Launch a goroutine to process results concurrently
    go func() {
        for result := range results {
            processCNAMEResult(result, fingerprints)
        }
    }()

    for scanner.Scan() {
        domain := scanner.Text()
        wg.Add(1)
        sem <- struct{}{} // Acquire semaphore

        // Launch a goroutine for each CNAME query
        go func(domain string) {
            defer wg.Done()
            defer func() { <-sem }() // Release semaphore
            queryAndSendCNAME(domain, results)
        }(domain)
    }

    if err := scanner.Err(); err != nil {
        log.Error("Error reading from file: ", err)
        return
    }

    // Wait for all queries to finish
    wg.Wait()

    // Close the results channel after all queries are complete
    close(results)

    // Write results after processing
    writeResults()
}

// Performs a CNAME query for a given domain and sends the result to the results channel
func queryAndSendCNAME(domain string, results chan<- cnameResult) {
    cname, err := exec.Command("dig", "+short", "CNAME", domain).Output()
    if err != nil || len(cname) == 0 {
        results <- cnameResult{domain: domain, err: fmt.Errorf("no CNAME record found or dig command failed")}
    } else {
        // Log the found CNAME
        log.Infof("CNAME found for %s is: %s", domain, strings.TrimSpace(string(cname)))
        results <- cnameResult{domain: domain, cname: strings.TrimSpace(string(cname))}
    }
}

// Processes CNAME query results from the results channel, sorting them into found and not found
func processResults(results <-chan cnameResult) {
	for result := range results {
		if result.err != nil || result.cname == "" {
			notFoundMsg := fmt.Sprintf("No CNAME record found for: %s", result.domain)
			log.Warnf(notFoundMsg)
			notFound = append(notFound, notFoundMsg)
		} else {
			foundMsg := fmt.Sprintf("CNAME for %s is: %s", result.domain, result.cname)
			log.Infof(foundMsg)
			found = append(found, foundMsg)
		}
	}

	log.Info("... Finished querying CNAMEs")
}

// Writes the sorted CNAME query results to an output markdown file with categorization based on exploitability
func writeResults() {
    outputFile, err := os.Create(outputFileName)
    if err != nil {
        log.Fatalf("Error creating output file: %v", err)
    }
    defer outputFile.Close()

    // Writing Is Exploitable section
    if len(isExploitable) > 0 {
        outputFile.WriteString("### Is Exploitable\n\n")
        for _, item := range isExploitable {
            outputFile.WriteString("- " + item + "\n")
        }
        outputFile.WriteString("\n")
    }

    // Writing Not Exploitable section
    if len(notExploitable) > 0 {
        outputFile.WriteString("### Not Exploitable\n\n")
        for _, item := range notExploitable {
            outputFile.WriteString("- " + item + "\n")
        }
        outputFile.WriteString("\n")
    }

    // Writing Exploitability Unknown section
    if len(unknownExploitability) > 0 {
        outputFile.WriteString("### Exploitability Unknown\n\n")
        for _, item := range unknownExploitability {
            outputFile.WriteString("- " + item + "\n")
        }
    }

    log.Println("Results have been written to", outputFileName)
}

// Searches for a CNAME in the fingerprints and checks its vulnerability status.
func isVulnerableCNAME(cname string, fingerprints map[string]map[string]interface{}) (bool, bool) {
    // Trim the trailing dot from the cname if present
    cname = strings.TrimSuffix(cname, ".")
    
    for _, fingerprint := range fingerprints {
        cnameList := fingerprint["cname"].([]interface{})
        for _, c := range cnameList {
            pattern := c.(string)
            if strings.HasSuffix(cname, pattern) {
                return true, fingerprint["vulnerable"].(bool)
            }
        }
    }
    return false, false // CNAME not found in fingerprints
}

// processCNAMEResult processes each CNAME query result, checking against fingerprints and service names
func processCNAMEResult(result cnameResult, fingerprints map[string]map[string]interface{}) {
    if result.err != nil || result.cname == "" {
        notFoundMsg := fmt.Sprintf("No CNAME record found for: %s", result.domain)
        log.Warnf(notFoundMsg)
        notFound = append(notFound, notFoundMsg)
        return
    }

    directMatch, vulnerable := isVulnerableCNAME(result.cname, fingerprints)
    if directMatch {
        foundMsg := fmt.Sprintf("CNAME for %s is: %s (found matching fingerprint - %s)", result.domain, result.cname, ifThenElse(vulnerable, "vulnerable", "safe"))
        appendResultBasedOnVulnerability(vulnerable, foundMsg)
    } else {
        // Handle the case where the service might be identified by its second-level domain in the fingerprints
        sld := extractServiceName(result.cname)
        if serviceMatch, vulnerable, service, fingerprintText, hasNXDOMAINFlag := isServiceVulnerable(sld, fingerprints); serviceMatch {
            serviceMsg := fmt.Sprintf("CNAME for %s is: %s (found potentially matching service '%s' - %s)", result.domain, result.cname, service, ifThenElse(vulnerable, "vulnerable", "safe"))
            appendResultBasedOnVulnerability(vulnerable, serviceMsg)
        } else {
            unknownMsg := fmt.Sprintf("CNAME for %s is: %s", result.domain, result.cname)
            unknownExploitability = append(unknownExploitability, unknownMsg)
        }
    }
}

// Checks if the domain pointed by the CNAME is take-over-able
func checkTakeover(domain string, fingerprintText string, hasNXDOMAINFlag bool) bool {
	if hasNXDOMAINFlag {
		return checkTakeoverDNS(domain, fingerprintText)
	} else {
		return checkTakeoverHTTP(domain, fingerprintText)
	}
}

// Checks if the domain pointed by the CNAME is take-over-able by performing a DNS query
func checkTakeoverDNS(domain string, fingerprintText string) bool {
    cname := fmt.Sprintf("_cname.%s", domain) // Construct the CNAME query
    _, err := net.LookupCNAME(cname)

    if err != nil {
        if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound { // Check if NXDOMAIN error
            return true // Take-over is possible
        }
        log.Errorf("Error performing DNS query for %s: %v", cname, err)
    }
    return false
}

func checkTakeoverHTTP(domain string, fingerprintText string) bool {
	url := "http://" + domain
	resp, err := http.Get(url)
	if err != nil {
		log.Errorf("Error making HTTP request to %s: %v", url, err)
		return false
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Errorf("Error reading response body from %s: %v", url, err)
		return false
	}

	// Check if the response body matches the fingerprint
	return strings.Contains(string(body), fingerprintText)
}
