// Command mincmon is a network ping monitor with a live terminal UI.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"mincmon/internal/app"
	"mincmon/internal/monitor"
	"mincmon/internal/probe"
	"mincmon/internal/store"
	"mincmon/internal/targets"
	"mincmon/internal/tui"
)

// stdinPrompter answers prompts from plain line input before the TUI starts.
type stdinPrompter struct{ r *bufio.Reader }

func (p stdinPrompter) Prompt(text string) (string, error) {
	fmt.Print(text)
	line, err := p.r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		fmt.Println()
		return "", app.ErrCancelled
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (p stdinPrompter) Notify(text string) { fmt.Println("  " + text) }

func main() {
	file := flag.String("f", "", "load hosts from a `file`"+store.Ext+" monitor list")
	interval := flag.Duration("i", 2*time.Second, "probe `interval` per host")
	timeout := flag.Duration("t", time.Second, "reply `timeout`")
	resolvers := flag.String("resolver", "", "DNS resolver `IPs` for domains, comma separated, or \"system\" (default: ask)")
	records := flag.String("records", "", "`types` to resolve for domains: A, AAAA or both (default: ask)")
	backend := flag.String("backend", "auto", "probe `backend`: auto, icmp, raw or exec")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [IP|CIDR|domain ...]\n\n"+
			"With no targets or -f, mincmon asks for them interactively.\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("mincmon", tui.Version)
		return
	}
	if *interval <= 0 || *timeout <= 0 {
		fail("interval and timeout must be positive")
	}

	dns := app.DNSOptions{}
	if *resolvers != "" {
		dns.ResolversSet = true
		if !strings.EqualFold(*resolvers, "system") {
			for _, r := range strings.Split(*resolvers, ",") {
				a, err := netip.ParseAddr(strings.TrimSpace(r))
				if err != nil {
					fail("invalid resolver %q", r)
				}
				dns.Resolvers = append(dns.Resolvers, a)
			}
		}
	}
	if *records != "" {
		dns.RecordsSet = true
		dns.WantA, dns.WantAAAA = app.ParseRecords(*records)
	}

	mode := probe.Mode(strings.ToLower(*backend))
	switch mode {
	case probe.Auto, probe.ICMP, probe.Raw, probe.Exec:
	default:
		fail("unknown backend %q", *backend)
	}
	prober, hint, err := probe.New(mode)
	if err != nil {
		fail("%v", err)
	}
	defer prober.Close()

	fmt.Printf("mincmon v%s  (probe: %s)\n\n", tui.Version, prober.Name())
	sp := stdinPrompter{bufio.NewReader(os.Stdin)}
	var ts []targets.Target

	if *file != "" {
		items, err := store.Load(*file)
		if err != nil {
			fail("%v", err)
		}
		ts = append(ts, items...)
	}
	input := strings.Join(flag.Args(), ",")
	if *file == "" && input == "" {
		input, err = sp.Prompt("Input IP addresses, subnets, and/or domains to monitor, " +
			"or enter 'load' to load an existing save:\n> ")
		if err != nil {
			fmt.Println("Exiting.")
			return
		}
		if strings.EqualFold(strings.TrimSpace(input), "load") {
			fmt.Println()
			items, msg := app.PromptLoad(sp)
			fmt.Println(msg)
			ts = append(ts, items...)
			input = ""
		}
	}
	if entries := targets.ParseEntries(input); len(entries) > 0 {
		resolved, err := app.ResolveEntries(entries, sp, dns)
		if err != nil {
			fmt.Println("Exiting.")
			return
		}
		ts = append(ts, resolved...)
	}
	ts = targets.Dedupe(ts)
	if len(ts) == 0 {
		fmt.Println("No target hosts resolved. Exiting.")
		return
	}

	mon := monitor.New(prober, *interval, *timeout)
	mon.Add(ts)
	err = tui.Run(mon, dns, hint)
	mon.Stop()
	if err != nil {
		fail("%v", err)
	}
	fmt.Println("Stopping monitor. Goodbye!")
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "mincmon: "+format+"\n", a...)
	os.Exit(1)
}
