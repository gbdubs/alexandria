package archive

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

// parsePlist decodes an XML property list, as printed by `diskutil info
// -plist` and `hdiutil info -plist`, into maps, slices, strings, int64s,
// float64s and bools.
func parsePlist(data []byte) (any, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if start, ok := token.(xml.StartElement); ok && start.Name.Local != "plist" {
			return plistValue(decoder, start)
		}
	}
}

func plistValue(decoder *xml.Decoder, start xml.StartElement) (any, error) {
	switch start.Name.Local {
	case "dict", "array":
		dict, array, key := map[string]any{}, []any{}, ""
		for {
			token, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			switch element := token.(type) {
			case xml.EndElement:
				if start.Name.Local == "dict" {
					return dict, nil
				}
				return array, nil
			case xml.StartElement:
				if start.Name.Local == "dict" && element.Name.Local == "key" {
					if err := decoder.DecodeElement(&key, &element); err != nil {
						return nil, err
					}
					continue
				}
				value, err := plistValue(decoder, element)
				if err != nil {
					return nil, err
				}
				if start.Name.Local == "dict" {
					dict[key] = value
				} else {
					array = append(array, value)
				}
			}
		}
	case "true", "false":
		return start.Name.Local == "true", decoder.Skip()
	}
	var text string
	if err := decoder.DecodeElement(&text, &start); err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	switch start.Name.Local {
	case "integer":
		return strconv.ParseInt(text, 10, 64)
	case "real":
		return strconv.ParseFloat(text, 64)
	}
	return text, nil
}

func plistString(values map[string]any, key string) string {
	text, _ := values[key].(string)
	return text
}

func plistBool(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}
