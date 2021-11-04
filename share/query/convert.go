package query

import (
	"fmt"
	"strings"
)

func ConvertListOptionsToQuery(lo *ListOptions, q string) (qOut string, params []interface{}) {
	qOut, params = addWhere(lo.Filters, q)
	qOut = addOrderBy(lo.Sorts, qOut)
	qOut = ReplaceStarSelect(lo.Fields, qOut)

	return qOut, params
}

func ConvertRetrieveOptionsToQuery(ro *RetrieveOptions, q string) string {
	qOut := ReplaceStarSelect(ro.Fields, q)

	return qOut
}

func AppendOptionsToQuery(o *ListOptions, q string, inParams []interface{}) (string, []interface{}) {
	qOut, params := addWhere(o.Filters, q)
	outParams := append(inParams, params...)
	qOut = addOrderBy(o.Sorts, qOut)
	qOut = ReplaceStarSelect(o.Fields, qOut)

	return qOut, outParams
}

func addWhere(filterOptions []FilterOption, q string) (qOut string, params []interface{}) {
	params = []interface{}{}
	if len(filterOptions) == 0 {
		return q, params
	}

	whereParts := make([]string, 0, len(filterOptions))
	for i := range filterOptions {
		if len(filterOptions[i].Values) == 1 {
			whereParts = append(whereParts, fmt.Sprintf("%s %s ?", filterOptions[i].Column, filterOptions[i].Operator.Code()))
			params = append(params, filterOptions[i].Values[0])
		} else {
			orParts := make([]string, 0, len(filterOptions[i].Values))
			for y := range filterOptions[i].Values {
				orParts = append(orParts, fmt.Sprintf("%s %s ?", filterOptions[i].Column, filterOptions[i].Operator.Code()))
				params = append(params, filterOptions[i].Values[y])
			}

			whereParts = append(whereParts, fmt.Sprintf("(%s)", strings.Join(orParts, " OR ")))
		}
	}

	concat := " WHERE "
	qUpper := strings.ToUpper(q)
	if strings.Contains(qUpper, " WHERE ") {
		concat = " AND "
	}
	q += concat + strings.Join(whereParts, " AND ") + " "

	return q, params
}

func addOrderBy(sortOptions []SortOption, q string) string {
	if len(sortOptions) == 0 {
		return q
	}
	orderByValues := make([]string, 0, len(sortOptions))
	for i := range sortOptions {
		direction := "ASC"
		if !sortOptions[i].IsASC {
			direction = "DESC"
		}
		orderByValues = append(orderByValues, fmt.Sprintf("%s %s", sortOptions[i].Column, direction))
	}
	if len(orderByValues) > 0 {
		q += "ORDER BY " + strings.Join(orderByValues, ", ")
	}

	return q
}

func ReplaceStarSelect(fieldOptions []FieldsOption, q string) string {
	if !strings.HasPrefix(strings.ToUpper(q), "SELECT * ") {
		return q
	}
	if len(fieldOptions) == 0 {
		return q
	}

	fields := []string{}
	for _, fo := range fieldOptions {
		for _, field := range fo.Fields {
			fields = append(fields, fmt.Sprintf("%s.%s", fo.Resource, field))
		}
	}

	return strings.Replace(q, "*", strings.Join(fields, ", "), 1)
}
