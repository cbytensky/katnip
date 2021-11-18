package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"reflect"
	"strings"
)

var HttpPort *string

func AddFlagHttp() {
	HttpPort = flag.String("httpport", "80", "HTTP server port")
}

func HttpServe() {
	http.Handle("/style.css", http.FileServer(http.Dir(".")))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body := "<table>\n" +
			"<caption>Latest blocks</caption>\n" +
			"<thead class=\"thead\">\n" +
			"<tr><th>Timestamp</th><th>Hash</th><th>Blue score</th></tr>\n" +
			"</thead>\n" +
			"<tbody>\n"
		latestNum := len(LatestHashes)
		var block Block
		for i := 0; i < latestNum; i++ {
			hash := LatestHashes[(LatestHashesTop+latestNum-i)%latestNum]
			if hash != nil && dbGet(PrefixBlock, (*hash)[:KeyLength], &block) {
				hashStr := H2s(*hash)
				body += "<a class=\"tr\" href=\"/block/" + hashStr[:KeyLength*2] + "\"><td>" + TimestampFormat(block.Timestamp) +
					"</td><td>" + hashStr + "</td><td>" + fmt.Sprintf("%d", block.BlueScore) +
					"</td></a>\n"
			}
		}
		w.Header().Set("Content-Type", "application/xhtml+xml")
		w.Write([]byte(Html(body +
			"</tbody>\n" +
			"</table>\n")))
	})
	http.HandleFunc("/block/", func(w http.ResponseWriter, r *http.Request) {
		Log(LogErr, "Path: %v", r.URL.Path)
		path := strings.Split(r.URL.Path, "/")
		if len(path) < 3 {
			HttpError(errors.New(fmt.Sprintf("Malformed path: %v", path)), "", w)
			return
		}
		hashSlice, err := hex.DecodeString(path[2])
		if err != nil {
			HttpError(errors.New(fmt.Sprintf("Bad hash: %v", path)), " decoding hash", w)
			return
		}
		block := Block{}
		if !dbGet(PrefixBlock, hashSlice[0:KeyLength], &block) {
			HttpError(errors.New("key not found: "+path[2]), "", w)
			return
		}

		body := "<table>\n" +
			"<tbody>\n"
		metaStruct := reflect.ValueOf(block)
		for i := 0; i < metaStruct.NumField(); i++ {
			field := metaStruct.Type().Field(i)
			metaValue := metaStruct.Field(i)
			value := metaValue.Interface()
			valueStr := fmt.Sprintf("%v", value)
			name := field.Name
			title := ToTitle(name)
			if hash, isHash := value.(Hash); isHash {
				valueStr = H2s(hash)
				//valueStr = "<a href=\"/block/" + valueStr + "\">" + valueStr + "</a>"
			}
			if name == "ParentLevels" {
				valueStr = ""
				numParents := 0
				if metaValue.Len() > 0 {
					parents := value.([][]uint64)[0]
					numParents = len(parents)
					for _, index := range parents {
						HashStr := H2s(block.Parents[index])
						Suffix := ""
						b1, b2 := "", ""
						if index == block.SelectedParent {
							//Suffix = " (selected)"
							b1, b2 = "<strong>", "</strong>"
						}
						valueStr += "<div><a href=\"/block/" + HashStr + "\">" + b1 + HashStr + b2 + "</a>" + Suffix + "</div>"
					}
				}
				title = fmt.Sprintf("Parents (%d)", numParents)
			}
			if name == "Parents" || name == "SelectedParent" {
				continue
			}
			if name == "PruningPoint" {
				for k, v := range PruningPointsStr {
					if v == block.PruningPoint {
						valueStr = "<div><a href=\"/block/" + k + "\">" + k + "</a></div>"
						break
					}
				}
			}
			if name == "Bits" {
				valueStr = fmt.Sprintf("%b", value)
			}
			if name == "Nonce" || name == "BlueWork" {
				valueStr = fmt.Sprintf("%x", value)
				if valueStr[0] == '0' {
					valueStr = valueStr[1:]
				}
			}
			if name == "Timestamp" {
				valueStr = TimestampFormat(value.(uint64))
			}
			if name == "TransactionIds" {
				title = fmt.Sprintf("Transactions (%d)", len(block.TransactionIds))
				valueStr = ""
				for _, transactionId := range block.TransactionIds {
					valueStr += fmt.Sprintf("<div><a href=\"/tx/%s\">%s</a></div>\n", B2s(transactionId[:KeyLength]), H2s(transactionId))
				}

			}
			body += fmt.Sprintf("<tr><th>%s</th><td>%s</td></tr>\n", title, valueStr)
		}
		body += "</tbody>\n" +
			"</table>\n"
		w.Write([]byte(Html(body)))

	})
	http.HandleFunc("/tx/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.Split(r.URL.Path, "/")
		if len(path) < 3 {
			HttpError(errors.New(fmt.Sprintf("Malformed path: %v", path)), "", w)
			return
		}
		hashSlice, err := hex.DecodeString(path[2])
		if err != nil {
			HttpError(errors.New(fmt.Sprintf("Bad hash: %v", path)), " decoding hash", w)
			return
		}
		transaction := Transaction{}
		if !dbGet(PrefixTransaction, hashSlice[0:KeyLength], &transaction) {
			HttpError(errors.New("key not found: "+path[2]), "", w)
			return
		}

		body := "<table>\n" +
			"<tbody>\n"
		metaStruct := reflect.ValueOf(transaction)
		for i := 0; i < metaStruct.NumField(); i++ {
			field := metaStruct.Type().Field(i)
			metaValue := metaStruct.Field(i)
			value := metaValue.Interface()
			valueStr := fmt.Sprintf("%v", value)
			name := field.Name
			title := ToTitle(name)
			if hash, isHash := value.(Hash); isHash {
				valueStr = H2s(hash)
			}
			if bytes, isBytes := value.(Bytes); isBytes {
				valueStr = B2s(bytes)
			}
			if name == "Inputs" || name == "Outputs" {
				continue
			}
			body += fmt.Sprintf("<tr><th>%s</th><td>%s</td></tr>\n", title, valueStr)
		}
		body += "</tbody>\n" +
			"</table>\n"
		body += "<table>\n" +
			"<caption>Inputs (" + fmt.Sprintf("%d", len(transaction.Inputs)) + ")</caption>\n" +
			"<tbody>\n" +
			"<tr>"
		metaStruct = reflect.ValueOf(Input{})
		for i := 0; i < metaStruct.NumField(); i++ {
			body += fmt.Sprintf("<th>%s</th>", ToTitle(metaStruct.Type().Field(i).Name))
		}
		body += "</tr>\n"
		for _, input := range transaction.Inputs {
			body += "<tr>"
			metaStruct := reflect.ValueOf(input)
			for i := 0; i < metaStruct.NumField(); i++ {
				field := metaStruct.Type().Field(i)
				metaValue := metaStruct.Field(i)
				value := metaValue.Interface()
				valueStr := fmt.Sprintf("%v", value)
				name := field.Name
				_ = name
				body += fmt.Sprintf("<td>%s</td>\n", valueStr)
			}
			body += "</tr>\n"
		}
		body += "</tbody>\n" +
			"</table>\n"
		body += "<table>\n" +
			"<caption>Outputs (" + fmt.Sprintf("%d", len(transaction.Outputs)) + ")</caption>\n" +
			"<tbody>\n" +
			"<tr>"
		metaStruct = reflect.ValueOf(Output{})
		for i := 0; i < metaStruct.NumField(); i++ {
			body += fmt.Sprintf("<th>%s</th>", ToTitle(metaStruct.Type().Field(i).Name))
		}
		body += "</tr>\n"
		for _, input := range transaction.Outputs {
			body += "<tr>"
			metaStruct := reflect.ValueOf(input)
			for i := 0; i < metaStruct.NumField(); i++ {
				field := metaStruct.Type().Field(i)
				metaValue := metaStruct.Field(i)
				value := metaValue.Interface()
				valueStr := fmt.Sprintf("%v", value)
				name := field.Name
				_ = name
				if hash, isHash := value.(Hash); isHash {
					valueStr = H2s(hash)
				}
				if bytes, isBytes := value.(Bytes); isBytes {
					valueStr = B2s(bytes)
				}
				body += fmt.Sprintf("<td>%s</td>\n", valueStr)
			}
			body += "</tr>\n"
		}
		body += "</tbody>\n" +
			"</table>\n"
		w.Write([]byte(Html(body)))
	})
	Log(LogInf, "HTTP server started, port: %s", *HttpPort)
	go http.ListenAndServe(":"+*HttpPort, nil)
}

func HttpError(err error, title string, w http.ResponseWriter) bool {
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(Html(
		"<h1>Error" + title + "</h1>\n" +
			"<p>" + fmt.Sprintf("%v", err) + "</p>\n")))
	return true
}

func Html(body string) string {
	return "<!DOCTYPE html>\n" +
		"<html xmlns=\"http://www.w3.org/1999/xhtml\" lang=\"en\">\n" +
		"<head>\n" +
		"<meta charset=\"UTF-8\"/>\n" +
		"<meta name=\"viewport\" content=\"width=device-width\"/>\n" +
		"<link rel=\"stylesheet\" href=\"/style.css\"/>\n" +
		"<title>Katnip</title>\n" +
		"</head>\n" +
		"<body>\n" +
		body +
		"</body>\n" +
		"</html>"
}

func isUpper(c byte) bool {
	return c >= 'A' && c <= 'Z'
}

func ToTitle(s string) string {
	r := make([]byte, 0, len(s)+2)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUpper(c) && i > 0 && i+1 < len(s) && !(isUpper(s[i-1]) && isUpper(s[i+1])) {
			r = append(r, ' ')
		}
		r = append(r, c)
	}
	return string(r)
}

func TrimZeroes(s string) string {
	var i int
	for i = 0; i < len(s); i++ {
		if s[i] != '0' {
			break
		}
	}
	s = s[i:]
	return s
}
