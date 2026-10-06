// Command mincmon is a network ping monitor with a live terminal UI.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
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
	os.Exit(run())
}

func run() int {
	file := flag.String("f", "", "load hosts from a `file`"+store.Ext+" monitor list")
	interval := flag.Duration("i", 2*time.Second, "probe `interval` per host")
	timeout := flag.Duration("t", time.Second, "reply `timeout`")
	resolvers := flag.String("resolver", "", "DNS resolver `IPs` for domains, comma separated, or \"system\" (default: ask)")
	records := flag.String("records", "", "`types` to resolve for domains: A, AAAA or both (default: ask)")
	backend := flag.String("backend", "auto", "probe `backend`: auto, icmp, raw or exec")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [flags] [IP|CIDR|domain ...]\n\n"+
			"With no targets or -f, mincmon asks for them interactively.\n\nFlags must come before targets.\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("mincmon", tui.Version)
		return 0
	}
	// stdlib flag stops at the first positional: "mincmon 10.0.0.1 -i 1s"
	// would silently treat "-i" as a domain. Detect that trap.
	for _, a := range flag.Args() {
		if strings.HasPrefix(a, "-") && a != "-" {
			fmt.Fprintf(os.Stderr, "mincmon: flags must come before targets (saw %q after a target)\n", a)
			flag.Usage()
			return 1
		}
	}
	if *interval <= 0 || *timeout <= 0 {
		return fail("interval and timeout must be positive")
	}

	dns := app.DNSOptions{}
	if *resolvers != "" {
		dns.ResolversSet = true
		if !strings.EqualFold(strings.TrimSpace(*resolvers), "system") {
			for _, r := range strings.Split(*resolvers, ",") {
				r = strings.TrimSpace(r)
				if r == "" {
					continue
				}
				// Tolerate host:port? Strip a trailing :port for now;
				// custom DNS ports are not yet supported (always 53).
				if ap, err := netip.ParseAddrPort(r); err == nil {
					r = ap.Addr().String()
				}
				a, err := netip.ParseAddr(r)
				if err != nil {
					return fail("invalid resolver %q", r)
				}
				dns.Resolvers = append(dns.Resolvers, a.WithZone(""))
			}
		}
	}
	if *records != "" {
		wantA, wantAAAA, err := app.ParseRecordsStrict(*records)
		if err != nil {
			return fail("%v", err)
		}
		dns.RecordsSet = true
		dns.WantA, dns.WantAAAA = wantA, wantAAAA
	}

	mode := probe.Mode(strings.ToLower(*backend))
	switch mode {
	case probe.Auto, probe.ICMP, probe.Raw, probe.Exec:
	default:
		return fail("unknown backend %q", *backend)
	}
	prober, hint, err := probe.New(mode)
	if err != nil {
		return fail("%v", err)
	}
	defer prober.Close()

	fmt.Printf("mincmon v%s  (probe: %s)\n\n", tui.Version, prober.Name())
	sp := stdinPrompter{bufio.NewReader(os.Stdin)}
	var ts []targets.Target

	if *file != "" {
		items, skipped, err := store.LoadWithStats(*file)
		if err != nil {
			return fail("%v", err) // deferred prober.Close() runs here
		}
		if skipped > 0 {
			fmt.Printf("  ! %d bad row(s) skipped in %s\n", skipped, *file)
		}
		ts = append(ts, items...)
	}
	input := strings.Join(flag.Args(), ",")
	if *file == "" && input == "" {
		input, err = sp.Prompt("Input IP addresses, subnets, and/or domains to monitor, " +
			"or enter 'load' to load an existing save:\n> ")
		if err != nil {
			fmt.Println("Exiting.")
			return 0
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
			return 0
		}
		ts = append(ts, resolved...)
	}
	ts = targets.Dedupe(ts)
	if len(ts) == 0 {
		fmt.Println("No target hosts resolved. Exiting.")
		return 0
	}

	mon := monitor.New(prober, *interval, *timeout)
	// Ensure Ctrl-C / SIGTERM during the TUI still stops workers cleanly.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		mon.Stop()
	}()
	mon.Add(ts)
	err = tui.Run(mon, dns, hint)
	mon.Stop()
	signal.Stop(sigCh)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Println("Stopping monitor. Goodbye!")
	return 0
}

func fail(format string, a ...any) int {
	fmt.Fprintf(os.Stderr, "mincmon: "+format+"\n", a...)
	return 1
}
