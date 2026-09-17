// tsql is the TSQL command-line client.
//
// Usage:
//
//	tsql [-h host] [-p port] [-c "SQL"]     one-shot statement
//	tsql                                     interactive REPL (\q quit, \dt tables)
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"github.com/meonglotong/tsql/internal/protocol"
)

func main() {
	cmd := flag.String("c", "", "run a single statement and exit")
	host := flag.String("h", "127.0.0.1", "server host")
	port := flag.Int("p", 5433, "server port")
	unixSock := flag.String("unix", "", "unix socket path (overrides -h/-p)")
	flag.Parse()

	var netName, addr string
	if *unixSock != "" {
		netName, addr = "unix", *unixSock
	} else {
		netName, addr = "tcp", fmt.Sprintf("%s:%d", *host, *port)
	}
	conn, err := net.DialTimeout(netName, addr, 5*1e9)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsql: connection to %s failed: %v\n", addr, err)
		os.Exit(2)
	}
	defer conn.Close()

	if *cmd != "" {
		os.Exit(runOnce(conn, *cmd))
	}
	repl(conn)
}

func runOnce(conn net.Conn, sql string) int {
	sql = strings.TrimSpace(sql)
	for strings.HasSuffix(sql, ";") {
		sql = strings.TrimSpace(sql[:len(sql)-1])
	}
	if sql == "" {
		return 0
	}
	msgs, err := sendQuery(conn, sql)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
		return 1
	}
	if !printResults(os.Stdout, msgs) {
		return 1
	}
	return 0
}

// sendQuery writes one query frame and reads until the ready frame.
func sendQuery(conn net.Conn, sql string) ([]protocol.Message, error) {
	if err := protocol.WriteFrame(conn, protocol.Message{Type: "query", SQL: sql}); err != nil {
		return nil, err
	}
	var msgs []protocol.Message
	for {
		m, err := protocol.ReadFrame(conn)
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, m)
		if m.Type == "ready" {
			return msgs, nil
		}
	}
}

func printResults(w io.Writer, msgs []protocol.Message) bool {
	failed := false
	for _, m := range msgs {
		switch m.Type {
		case "result":
			if len(m.Rows) == 0 {
				fmt.Fprintln(w, "(0 rows)")
				continue
			}
			fmt.Fprint(w, formatTable(m.Columns, m.Rows))
			fmt.Fprintf(w, "(%d row%s)\n", len(m.Rows), plural(len(m.Rows)))
		case "ok":
			fmt.Fprintf(w, "OK (%d row%s)\n", m.Affected, plural(m.Affected))
		case "error":
			fmt.Fprintf(os.Stderr, "ERROR [%s]: %s\n", m.Code, m.Message)
			failed = true
		}
	}
	return !failed
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// formatTable renders psql-style aligned output.
func formatTable(cols []string, rows [][]any) string {
	cells := make([][]string, len(rows))
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	for i, r := range rows {
		cells[i] = make([]string, len(cols))
		for j := 0; j < len(cols); j++ {
			s := ""
			if j < len(r) && r[j] != nil {
				s = fmt.Sprintf("%v", r[j])
			}
			cells[i][j] = s
			if len(s) > widths[j] {
				widths[j] = len(s)
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(joinPadded(cols, widths))
	sb.WriteString("\n")
	var sep []string
	for j := range cols {
		sep = append(sep, strings.Repeat("-", widths[j]))
	}
	sb.WriteString(strings.Join(sep, "-") + "\n")
	for _, row := range cells {
		sb.WriteString(joinPadded(row, widths))
		sb.WriteString("\n")
	}
	return sb.String()
}

func joinPadded(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, s := range cells {
		if i < len(widths) && len(s) < widths[i] {
			s += strings.Repeat(" ", widths[i]-len(s))
		}
		parts[i] = s
	}
	return strings.Join(parts, " | ")
}

// classifyLine maps one REPL line to an action:
//
//	quit     \q, quit, exit
//	noop     empty line or bare semicolons
//	tables   \dt / /dt / \dn (v1 has one schema, \dn lists tables)
//	databases \l / \l+ / /l
//	describe \d [table] / \d+ [table] / /d [table]
//	help     \h, \help
//	unknown  any other backslash/forward-slash command
//	sql      everything else, with trailing semicolons stripped
//
// Forward slash is accepted as an alias for backslash (muscle memory).
func classifyLine(line string) (kind, arg string) {
	if strings.HasPrefix(line, "\\") || strings.HasPrefix(line, "/") {
		body := strings.TrimSpace(line[1:])
		parts := strings.SplitN(body, " ", 2)
		cmd := strings.ToLower(parts[0])
		cmd = strings.TrimSuffix(cmd, "+")
		a := ""
		if len(parts) > 1 {
			a = strings.TrimSpace(parts[1])
		}
		switch cmd {
		case "q":
			return "quit", ""
		case "dt", "dn":
			return "tables", ""
		case "l":
			return "databases", ""
		case "d":
			return "describe", a
		case "h", "help":
			return "help", ""
		default:
			return "unknown", body
		}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "quit", "exit":
		return "quit", ""
	}
	sql := strings.TrimSpace(line)
	for strings.HasSuffix(sql, ";") {
		sql = strings.TrimSpace(sql[:len(sql)-1])
	}
	if sql == "" {
		return "noop", ""
	}
	return "sql", sql
}

// logoASCII is the TSQL logo (ASCII version of docs/logo.png), shown in the REPL.
const logoASCII = `                   @@@@@@@@@@@@@@@@
               @@@@@@@@@@@@@@@@@@@@@
            @@@@@@@@@@@@@@@@@@@@@@@@@@@
          @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
        @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
       @@@@@@@@@@@@@@@@@@@@@@@@@@..@@@@@@@@@
       @@@@@@@@@@@@@@@@........@@..@@@@@@@@@@
      @@@@@@@@@.@@@@@@@.........@@@@@@@@@@@@@
      @@@@@@@@...@@@@@@........@@@@@@..@@@@@@
      @@@@@@@@....@@@@@.....@..@@@@@..@@@@@@@
      @@@@@@.@@....@@@......@@@@@@@....@@@@@@@
     @.@@@@.......@@@@@@.....@@@@@@.......@@@@@
     ....@..........@@@........@@@.........@..@@
     @................@.::....:.@@..........@@@
      @@@...................................@@@
       @@...................................@@
        @@..................................@
       @@@.................................@@
     @@@@@@@@...........................@@@@@@@
  @@@@@@@@@@@@@@.....................@@@@@@@@@@@@@
 @@@@@@@@@@@@@@@@@@@.............@@@@@@@@@@@@@@@@@@@
@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
  @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
     @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
         @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@
          @@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@@`

func repl(conn net.Conn) {
	fmt.Print(logoASCII + "\n")
	fmt.Println("tsql — TSQL interactive terminal (\\q quit, \\dt tables, \\d describe, \\h help)")
	in := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("tsql> ")
		line, err := in.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println()
			}
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		kind, arg := classifyLine(line)
		switch kind {
		case "quit":
			return
		case "noop":
			continue
		case "tables":
			listTables(conn)
			continue
		case "databases":
			listDatabases(conn)
			continue
		case "describe":
			if arg == "" {
				listTables(conn)
			} else {
				describeTable(conn, arg)
			}
			continue
		case "help":
			printHelp()
			continue
		case "unknown":
			fmt.Fprintf(os.Stderr, "tsql: unknown command %q (try \\h)\n", line)
			continue
		}
		msgs, err := sendQuery(conn, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsql: connection error: %v\n", err)
			return
		}
		printResults(os.Stdout, msgs)
	}
}

func printHelp() {
	fmt.Println(`commands:
  \q           quit
  \dt          list tables
  \dn          list schemas (v1: single schema, lists tables)
  \l           list databases (v1: single database)
  \d [table]   describe a table (no arg: list tables)
  \h           this help
SQL statements take one line each; a trailing semicolon is optional.`)
}

// listDatabases shows the v1 database list (a single database: tsql).
func listDatabases(conn net.Conn) {
	if err := protocol.WriteFrame(conn, protocol.Message{Type: "databases"}); err != nil {
		fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
		return
	}
	for {
		m, err := protocol.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
			return
		}
		if m.Type == "databases" {
			if len(m.Tables) == 0 {
				fmt.Println("(no databases)")
				return
			}
			for _, d := range m.Tables {
				fmt.Println(d)
			}
			return
		}
		if m.Type == "ready" {
			return
		}
	}
}

// describeTable prints a table's column listing.
func describeTable(conn net.Conn, name string) {
	if err := protocol.WriteFrame(conn, protocol.Message{Type: "describe", Name: name}); err != nil {
		fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
		return
	}
	for {
		m, err := protocol.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
			return
		}
		if m.Type == "describe" {
			fmt.Printf("%s\n", m.Name)
			if len(m.Rows) == 0 {
				fmt.Println("(0 rows)")
				return
			}
			fmt.Fprint(os.Stdout, formatTable(m.Columns, m.Rows))
			fmt.Fprintf(os.Stdout, "(%d row%s)\n", len(m.Rows), plural(len(m.Rows)))
			return
		}
		if m.Type == "error" {
			fmt.Fprintf(os.Stderr, "ERROR [%s]: %s\n", m.Code, m.Message)
			return
		}
		if m.Type == "ready" {
			return
		}
	}
}

func listTables(conn net.Conn) {
	if err := protocol.WriteFrame(conn, protocol.Message{Type: "tables"}); err != nil {
		fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
		return
	}
	for {
		m, err := protocol.ReadFrame(conn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tsql: %v\n", err)
			return
		}
		if m.Type == "tables" {
			if len(m.Tables) == 0 {
				fmt.Println("(no tables)")
				return
			}
			for _, t := range m.Tables {
				fmt.Println(t)
			}
			return
		}
		if m.Type == "ready" {
			return
		}
	}
}
