package reporting

import (
	"bytes"
	"html/template"
	"os"

	"github.com/activecm/rita/pkg/beaconsni"
	"github.com/activecm/rita/reporting/templates"
	"github.com/activecm/rita/resources"
)

func printBeaconsSNI(db string, showNetNames bool, res *resources.Resources, logsGeneratedAt string) error {
	var w string
	f, err := os.Create("beaconssni.html")
	if err != nil {
		return err
	}
	defer f.Close()

	var beaconsSNITempl string
	if showNetNames {
		beaconsSNITempl = templates.BeaconsSNINetNamesTempl
	} else {
		beaconsSNITempl = templates.BeaconsSNITempl
	}

	out, err := template.New("beaconssni.html").Parse(beaconsSNITempl)
	if err != nil {
		return err
	}

	data, err := beaconsni.Results(res, 0)
	if err != nil {
		return err
	}

	if len(data) == 0 {
		w = ""
	} else {
		w, err = getBeaconSNIWriter(data, showNetNames)
		if err != nil {
			return err
		}
	}

	return out.Execute(f, &templates.ReportingInfo{DB: db, Writer: template.HTML(w), LogsGeneratedAt: logsGeneratedAt})
}

func getBeaconSNIWriter(beaconsSNI []beaconsni.Result, showNetNames bool) (string, error) {
	tmpl := "<tr>"

	tmpl += "<td>{{printf \"%.3f\" .Score}}</td>"

	if showNetNames {
		tmpl += "<td>{{.SrcNetworkName}}</td><td>{{.SrcIP}}</td><td>{{.FQDN}}</td>"
	} else {
		tmpl += "<td>{{.SrcIP}}</td><td>{{.FQDN}}</td>"
	}
	tmpl += "<td>{{.Connections}}</td><td>{{printf \"%.3f\" .AvgBytes}}</td><td>"
	tmpl += "{{.Ts.Range}}</td><td>{{.Ds.Range}}</td><td>{{.Ts.Mode}}</td><td>{{.Ds.Mode}}</td><td>{{.Ts.ModeCount}}</td><td>{{.Ds.ModeCount}}</td><td>"
	tmpl += "{{printf \"%.3f\" .Ts.Skew}}</td><td>{{printf \"%.3f\" .Ds.Skew}}</td><td>{{.Ts.Dispersion}}</td><td>{{.Ds.Dispersion}}</td>"
	tmpl += "</tr>\n"

	out, err := template.New("beaconsni").Parse(tmpl)
	if err != nil {
		return "", err
	}

	w := new(bytes.Buffer)

	for _, result := range beaconsSNI {
		err = out.Execute(w, result)
		if err != nil {
			return "", err
		}
	}

	return w.String(), nil
}
