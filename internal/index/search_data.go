package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/spq/pkappa2/internal/query"
	"github.com/spq/pkappa2/internal/tools/bitmask"
	regexanalysis "github.com/spq/pkappa2/internal/tools/regexAnalysis"
	"github.com/spq/pkappa2/internal/tools/seekbufio"
	"golang.org/x/exp/slices"
	"rsc.io/binaryregexp"
)

type (
	occ struct {
		condition, element int
	}
	regexVariant struct {
		regex          *binaryregexp.Regexp
		prefix, suffix []byte
		acceptedLength regexanalysis.AcceptedLengths
		childSubQuery  string
		children       []regexVariant
		isPrecondition bool
	}
	regex struct {
		occurence []occ
		root      regexVariant
	}

	dataConditionsContainer struct {
		conditions   []*query.DataCondition
		regexes      []regex
		dependencies map[string]map[string]struct{}
	}

	variableValues struct {
		quotedData []string
		results    bitmask.ConnectedBitmask
	}
	subQueryVariableData struct {
		variableIndex map[string]int
		variableData  []variableValues
	}

	progressVariantFlag byte
	progressVariant     struct {
		streamOffset [2]int
		// how many regexes were sucessful
		nSuccessful int
		// the variables collected on the way
		variables map[string]string
		// the regex to use
		regex *binaryregexp.Regexp
		// the accepted length by the regex
		acceptedLength regexanalysis.AcceptedLengths
		// the prefix of the regex
		prefix []byte
		// the suffix of the regex
		suffix []byte
		// the variants chosen for this progress
		variant map[string]int
		// flags for this progress
		flags progressVariantFlag
	}
	progressGroup struct {
		variants []progressVariant
	}
)

const (
	progressVariantFlagState                    progressVariantFlag = 3
	progressVariantFlagStateUninitialzed        progressVariantFlag = 0
	progressVariantFlagStateExact               progressVariantFlag = 1
	progressVariantFlagStatePrecondition        progressVariantFlag = 2
	progressVariantFlagStatePreconditionMatched progressVariantFlag = 3

	C2S = query.DataRequirementSequenceFlagsDirectionClientToServer / query.DataRequirementSequenceFlagsDirection
	S2C = query.DataRequirementSequenceFlagsDirectionServerToClient / query.DataRequirementSequenceFlagsDirection
)

func (dcc *dataConditionsContainer) add(cc *query.DataCondition, subQuery string, previousResults map[string]resultData) error {
	if len(cc.Elements) == 0 {
		return nil
	}
	converterName := cc.Elements[0].ConverterName
	if len(dcc.conditions) != 0 {
		if converterName != dcc.conditions[0].Elements[0].ConverterName {
			return errors.New("all data conditions must have the same converter name")
		}
	}
	shouldEvaluate, affectsSubquery := false, false
	for _, e := range cc.Elements {
		if e.ConverterName != converterName {
			return errors.New("all data conditions must have the same converter name")
		}
		if e.SubQuery != subQuery {
			if _, ok := previousResults[e.SubQuery]; !ok {
				return nil
			}
			affectsSubquery = true
			continue
		}
		shouldEvaluate = true
		for _, v := range e.Variables {
			if _, ok := previousResults[v.SubQuery]; v.SubQuery != subQuery && !ok {
				return errors.New("SubQueries not yet fully supported")
			}
		}
	}
	if !shouldEvaluate {
		return nil
	}
	if affectsSubquery {
		return errors.New("SubQueries not yet fully supported")
	}
regexElements:
	for eIdx, e := range cc.Elements {
		for rIdx := range dcc.regexes {
			r := &dcc.regexes[rIdx]
			o := r.occurence[0]
			oe := dcc.conditions[o.condition].Elements[o.element]
			if e.Regex != oe.Regex {
				continue
			}
			if !slices.Equal(e.Variables, oe.Variables) {
				continue
			}
			r.occurence = append(r.occurence, occ{
				condition: len(dcc.conditions),
				element:   eIdx,
			})
			continue regexElements
		}
		for _, v := range e.Variables {
			if v.SubQuery == "" {
				continue
			}
			if _, ok := previousResults[v.SubQuery]; !ok {
				return errors.New("SubQueries not yet fully supported")
			}
			dep := dcc.dependencies[v.SubQuery]
			if dep == nil {
				dep = make(map[string]struct{})
			}
			dep[v.Name] = struct{}{}
			if dcc.dependencies == nil {
				dcc.dependencies = map[string]map[string]struct{}{}
			}
			dcc.dependencies[v.SubQuery] = dep
		}
		dcc.regexes = append(dcc.regexes, regex{
			occurence: []occ{{
				condition: len(dcc.conditions),
				element:   eIdx,
			}},
		})
	}
	dcc.conditions = append(dcc.conditions, cc)
	return nil
}

func (dcc *dataConditionsContainer) finalize(r *Reader, queryPartIndex int, previousResults map[string]resultData, converters map[string]ConverterAccess) ([]func(sc *searchContext, s *stream) (bool, error), error) {
	if len(dcc.conditions) == 0 {
		return alwaysSuccess, nil
	}
	converterName := dcc.conditions[0].Elements[0].ConverterName
	if converterName != "" && converterName != "none" {
		if _, ok := converters[converterName]; !ok {
			return nil, fmt.Errorf("converter %q not found", converterName)
		}
	}
	//sort the regexes
	for rIdx := range dcc.regexes {
		r := &dcc.regexes[rIdx]
		sort.Slice(r.occurence, func(il, ir int) bool {
			ol, or := r.occurence[il], r.occurence[ir]
			if ol.element != or.element {
				return ol.element < or.element
			}
			return ol.condition < or.condition
		})
	}
	sort.Slice(dcc.regexes, func(il, ir int) bool {
		ol, or := dcc.regexes[il].occurence[0], dcc.regexes[ir].occurence[0]
		if ol.element != or.element {
			return ol.element < or.element
		}
		return ol.condition < or.condition
	})

	impossibleSubQueries := map[string]*bitmask.ConnectedBitmask{}
	possibleSubQueries := map[string]subQueryVariableData{}
	for sq, vars := range dcc.dependencies {
		varNameIndex := make(map[string]int)
		for v := range vars {
			varNameIndex[v] = len(varNameIndex)
		}
		rd := previousResults[sq]
		badVarData := map[int]struct{}{}
		varData := []variableValues(nil)
		varDataMap := map[int]int{}
	vardata:
		for vdi := range rd.variableData {
			vd := &rd.variableData[vdi]
			if vd.uses == 0 {
				continue
			}
			quotedData := make([]string, len(varNameIndex))
			for v, vIdx := range varNameIndex {
				quoted := ""
				for _, d := range vd.data {
					if d.queryParts.IsSet(uint(queryPartIndex)) && d.name != v {
						continue
					}
					quoted += binaryregexp.QuoteMeta(d.value) + "|"
				}
				if quoted == "" {
					badVarData[vdi] = struct{}{}
					continue vardata
				}
				quotedData[vIdx] = quoted[:len(quoted)-1]
			}
		varDataElement:
			for i := range varData {
				vde := &varData[i]
				for j := range quotedData {
					if quotedData[j] != vde.quotedData[j] {
						continue varDataElement
					}
				}
				varDataMap[vdi] = i
				continue vardata
			}
			varDataMap[vdi] = len(varData)
			varData = append(varData, variableValues{
				quotedData: quotedData,
			})
		}
		possible := false
		impossible := &bitmask.ConnectedBitmask{}
		for sIdx, s := range rd.streams {
			if vdi, ok := rd.variableAssociation[s.StreamID]; ok {
				if _, ok := badVarData[vdi]; !ok {
					varData[varDataMap[vdi]].results.Set(uint(sIdx))
					possible = true
					continue
				}
			}
			// this stream can not succeed as it does not have the right variables
			impossible.Set(uint(sIdx))
		}
		if !possible {
			return alwaysFail, nil
		}
		if !impossible.IsZero() {
			impossibleSubQueries[sq] = impossible
		}
		possibleSubQueries[sq] = subQueryVariableData{
			variableIndex: varNameIndex,
			variableData:  varData,
		}
	}
	for rIdx := range dcc.regexes {
		r := &dcc.regexes[rIdx]
		o := &r.occurence[0]
		c := dcc.conditions[o.condition]
		e := &c.Elements[o.element]
		if len(e.Variables) == 0 {
			var err error
			if r.root.regex, err = binaryregexp.Compile(e.Regex); err != nil {
				return nil, err
			}
			prefix, complete := r.root.regex.LiteralPrefix()
			r.root.prefix = []byte(prefix)
			if complete {
				r.root.acceptedLength = regexanalysis.AcceptedLengths{
					MinLength: uint(len(prefix)),
					MaxLength: uint(len(prefix)),
				}
				r.root.suffix = r.root.prefix
			} else {
				if r.root.acceptedLength, err = regexanalysis.AcceptedLength(e.Regex); err != nil {
					return nil, err
				}
				if r.root.suffix, err = regexanalysis.ConstantSuffix(e.Regex); err != nil {
					return nil, err
				}
			}
			continue
		}

		precomputeSubQueries := []string{""}
		usesLocalVariables := false
	variables:
		for _, v := range e.Variables {
			if v.SubQuery == "" {
				usesLocalVariables = true
				continue
			}
			for _, sq := range precomputeSubQueries[1:] {
				if sq == v.SubQuery {
					continue variables
				}
			}
			precomputeSubQueries = append(precomputeSubQueries, v.SubQuery)
		}
		variantCount := map[string]int{
			"": 1,
		}
		for _, sq := range precomputeSubQueries[1:] {
			variantCount[sq] = len(possibleSubQueries[sq].variableData)
		}
		if usesLocalVariables {
			precomputeSubQueries = precomputeSubQueries[:1]
		} else {
			sort.Slice(precomputeSubQueries[1:], func(i, j int) bool {
				a, b := precomputeSubQueries[i+1], precomputeSubQueries[j+1]
				return variantCount[a] < variantCount[b]
			})
			count := 1
			for l, sq := range precomputeSubQueries[1:] {
				if count >= 10_000 {
					precomputeSubQueries = precomputeSubQueries[:l+1]
					break
				}
				count *= variantCount[sq]
			}
		}
		for depth := range precomputeSubQueries {
			position := make([]int, depth+1)

		variants:
			for {
				isPrecondition := false
				regex := e.Regex
				for i := len(e.Variables) - 1; i >= 0; i-- {
					v := e.Variables[i]
					content := ""
					if v.SubQuery == "" {
						//TODO: maybe extract the regex for this variable
						content = ".*"
						isPrecondition = true
					} else {
						psq := possibleSubQueries[v.SubQuery]
						vdMin, vdMax := 0, variantCount[v.SubQuery]
						for pIdx, sq := range precomputeSubQueries[1 : depth+1] {
							if v.SubQuery == sq {
								pos := position[pIdx+1]
								vdMin, vdMax = pos, pos+1
								break
							}
						}
						vIdx := psq.variableIndex[v.Name]
						for vdIdx := vdMin; vdIdx < vdMax; vdIdx++ {
							content += psq.variableData[vdIdx].quotedData[vIdx] + "|"
						}
						content = content[:len(content)-1]
						if vdMax-vdMin != 1 {
							isPrecondition = true
						}
					}
					regex = regex[:v.Position] + "(?:" + content + ")" + regex[v.Position:]
				}
				root := &r.root
				for _, p := range position[1:] {
					root = &root.children[p]
				}
				if depth+1 < len(precomputeSubQueries) {
					root.childSubQuery = precomputeSubQueries[depth+1]
					root.children = make([]regexVariant, variantCount[root.childSubQuery])
				}

				var err error
				if root.regex, err = binaryregexp.Compile(regex); err != nil {
					return nil, err
				}
				prefix, complete := root.regex.LiteralPrefix()
				root.prefix = []byte(prefix)
				if complete {
					root.acceptedLength = regexanalysis.AcceptedLengths{
						MinLength: uint(len(prefix)),
						MaxLength: uint(len(prefix)),
					}
					root.suffix = root.prefix
				} else {
					if root.acceptedLength, err = regexanalysis.AcceptedLength(regex); err != nil {
						return nil, err
					}
					if root.suffix, err = regexanalysis.ConstantSuffix(regex); err != nil {
						return nil, err
					}
				}
				root.isPrecondition = isPrecondition

				for pIdx := range position[1:] {
					pIdx++
					p := &position[pIdx]
					*p++
					if *p < variantCount[precomputeSubQueries[pIdx]] {
						continue variants
					}
					*p = 0
				}
				break
			}
		}
	}

	filters := []func(sc *searchContext, s *stream) (bool, error)(nil)

	//add filter for removing impossible subqueries
	if len(impossibleSubQueries) != 0 {
		filters = append(filters, func(sc *searchContext, _ *stream) (bool, error) {
			for sq, imp := range impossibleSubQueries {
				sc.allowedSubQueries.remove([]string{sq}, []*bitmask.ConnectedBitmask{imp})
			}
			return !sc.allowedSubQueries.empty(), nil
		})
	}

	//add filter for scanning the data section
	br := seekbufio.NewSeekableBufferReader(r.sectionReader(sectionData))
	buffers := [2][]byte{}
	convertersToSearch := []string(nil)
	if converterName == "" {
		convertersToSearch = append(convertersToSearch, "none")
		for c := range converters {
			convertersToSearch = append(convertersToSearch, c)
		}
	} else {
		convertersToSearch = []string{converterName}
	}
	return append(filters, func(sc *searchContext, s *stream) (bool, error) {
		for _, converterName := range convertersToSearch {
			ok, err := func() (bool, error) {
				progressGroups := make([]progressGroup, len(dcc.conditions))
				for i := range progressGroups {
					progressGroups[i].variants = make([]progressVariant, 1)
				}

				streamLength := [2]int{}
				bufferLengths := [][2]int{{}}

				if converterName == "none" {
					streamLength[C2S] = int(s.ClientBytes)
					streamLength[S2C] = int(s.ServerBytes)

					// read the data
					if _, err := br.Seek(int64(s.DataStart), io.SeekStart); err != nil {
						return false, err
					}
					for dir := range [2]int{C2S, S2C} {
						l := streamLength[dir]
						if cap(buffers[dir]) < l {
							buffers[dir] = make([]byte, l)
						} else {
							buffers[dir] = buffers[dir][:l]
						}
						if err := binary.Read(br, binary.LittleEndian, buffers[dir]); err != nil {
							return false, err
						}
					}
					// read the direction chunk sizes
					for dir := C2S; ; dir ^= C2S ^ S2C {
						last := bufferLengths[len(bufferLengths)-1]
						if last[C2S] == streamLength[C2S] && last[S2C] == streamLength[S2C] {
							break
						}
						sz := uint64(0)
						for {
							b := byte(0)
							if err := binary.Read(br, binary.LittleEndian, &b); err != nil {
								return false, err
							}
							sz <<= 7
							sz |= uint64(b & 0x7f)
							if b < 128 {
								break
							}
						}
						if sz == 0 {
							continue
						}
						new := [2]int{
							last[0],
							last[1],
						}
						new[dir] += int(sz)
						bufferLengths = append(bufferLengths, new)
					}
				} else {
					converter := converters[converterName]
					// TODO: pass `buffers` through to DataForSearch to avoid re-allocating?
					data, dataSizes, clientBytes, serverBytes, wasCached, err := converter.DataForSearch(s.StreamID)
					if err != nil {
						return false, fmt.Errorf("data for search %w", err)
					}
					if !wasCached {
						return false, nil
					}
					streamLength[C2S] = int(clientBytes)
					streamLength[S2C] = int(serverBytes)
					buffers = data
					bufferLengths = dataSizes
				}
				for {
					recheckRegexes := false
					for rIdx := range dcc.regexes {
						r := &dcc.regexes[rIdx]
						for _, o := range r.occurence {
							e := dcc.conditions[o.condition].Elements[o.element]
							dir := (e.Flags & query.DataRequirementSequenceFlagsDirection) / query.DataRequirementSequenceFlagsDirection

							ps := &progressGroups[o.condition]
						outer2:
							for pIdx := 0; pIdx < len(ps.variants); pIdx++ {
								p := &ps.variants[pIdx]
								if o.element != p.nSuccessful {
									continue
								}
								if p.regex == nil {
									root := &r.root
									for {
										if root.childSubQuery == "" {
											break
										}
										v, ok := p.variant[root.childSubQuery]
										if !ok {
											break
										}
										root = &root.children[v]
									}
									explodeOneVariant := false
									switch p.flags & progressVariantFlagState {
									case progressVariantFlagStateUninitialzed:
										if root.regex != nil {
											p.regex = root.regex
											p.prefix = root.prefix
											p.suffix = root.suffix
											p.acceptedLength = root.acceptedLength
											if root.isPrecondition {
												p.flags = progressVariantFlagStatePrecondition
											} else {
												p.flags = progressVariantFlagStateExact
											}
										}
									case progressVariantFlagStateExact:
										panic("why am i here?")
									case progressVariantFlagStatePrecondition:
										panic("why am i here?")
									case progressVariantFlagStatePreconditionMatched:
										if root.childSubQuery == "" {
											explodeOneVariant = true
											break
										}
										for cIdx, c := range root.children[1:] {
											np := progressVariant{
												streamOffset:   p.streamOffset,
												nSuccessful:    p.nSuccessful,
												regex:          c.regex,
												acceptedLength: c.acceptedLength,
												prefix:         c.prefix,
												suffix:         c.suffix,
												variant: map[string]int{
													root.childSubQuery: cIdx,
												},
											}
											for sq, v := range p.variant {
												np.variant[sq] = v
											}
											if p.variables != nil {
												np.variables = make(map[string]string)
												for n, v := range p.variables {
													np.variables[n] = v
												}
											}
											if c.isPrecondition {
												np.flags = progressVariantFlagStatePrecondition
											} else {
												np.flags = progressVariantFlagStateExact
											}
											if cIdx == 0 {
												ps.variants[pIdx] = np
											} else {
												ps.variants = append(ps.variants, np)
											}
										}
										p = &ps.variants[pIdx]
									}

									if p.regex == nil {
										expr := e.Regex
										p.flags = progressVariantFlagStateExact
										for i := len(e.Variables) - 1; i >= 0; i-- {
											v := e.Variables[i]
											content := ""
											if v.SubQuery == "" {
												ok := false
												content, ok = p.variables[v.Name]
												if !ok {
													return false, fmt.Errorf("variable %q not defined", v.Name)
												}
												content = binaryregexp.QuoteMeta(content)
											} else {
												psq := possibleSubQueries[v.SubQuery]
												vIdx := psq.variableIndex[v.Name]
												variant, ok := p.variant[v.SubQuery]
												if ok || explodeOneVariant {
													if !ok {
														explodeOneVariant = false
														// we have not yet split this progress element
														// the precondition regex matched, split this progress element
														for j := 1; j < len(psq.variableData); j++ {
															np := progressVariant{
																streamOffset: p.streamOffset,
																nSuccessful:  p.nSuccessful,
																flags:        progressVariantFlagStateUninitialzed,
																variant:      map[string]int{v.SubQuery: j},
															}
															for k, v := range p.variant {
																np.variant[k] = v
															}
															if p.variables != nil {
																np.variables = make(map[string]string)
																for n, v := range p.variables {
																	np.variables[n] = v
																}
															}
															ps.variants = append(ps.variants, np)
														}
														p = &ps.variants[pIdx]
														if p.variant == nil {
															p.variant = make(map[string]int)
														}
														p.variant[v.SubQuery] = 0
													}
													content = psq.variableData[variant].quotedData[vIdx]
												} else {
													p.flags = progressVariantFlagStatePrecondition
													for _, vd := range psq.variableData {
														content += vd.quotedData[vIdx] + "|"
													}
													content = content[:len(content)-1]
												}
											}
											expr = fmt.Sprintf("%s(?:%s)%s", expr[:v.Position], content, expr[v.Position:])
										}
										var err error
										if p.regex, err = binaryregexp.Compile(expr); err != nil {
											return false, err
										}
										prefix, complete := p.regex.LiteralPrefix()
										root.prefix = []byte(prefix)
										if complete {
											p.acceptedLength = regexanalysis.AcceptedLengths{
												MinLength: uint(len(prefix)),
												MaxLength: uint(len(prefix)),
											}
											root.suffix = root.prefix
										} else {
											if p.acceptedLength, err = regexanalysis.AcceptedLength(expr); err != nil {
												return false, err
											}
											if p.suffix, err = regexanalysis.ConstantSuffix(expr); err != nil {
												return false, err
											}
										}
									}
								}

								buffer := buffers[dir][p.streamOffset[dir]:]
								if uint(len(buffer)) < p.acceptedLength.MinLength {
									continue
								}

								if len(p.prefix) != 0 {
									//the regex has a prefix, find it
									pos := bytes.Index(buffer, p.prefix)
									if pos < 0 {
										// the prefix is not in the string, we can discard part of the buffer
										p.streamOffset[dir] = len(buffers[dir])
										continue
									}
									//skip the part that doesn't have the prefix
									p.streamOffset[dir] += pos
									buffer = buffer[pos:]
									if uint(len(buffer)) < p.acceptedLength.MinLength {
										continue
									}
								}
								if len(p.suffix) != 0 {
									//the regex has a suffix, find it
									pos := bytes.LastIndex(buffer, p.suffix)
									if pos < 0 {
										// the suffix is not in the string, we can discard part of the buffer
										p.streamOffset[dir] = len(buffers[dir])
										continue
									}
									//drop the part that doesn't have the suffix
									buffer = buffer[:pos+len(p.suffix)]
									if uint(len(buffer)) < p.acceptedLength.MinLength {
										continue
									}
								}

								var res []int
								if p.acceptedLength.MinLength == p.acceptedLength.MaxLength && len(p.prefix) == 0 && len(p.suffix) != 0 {
									beforeSuffixLen := int(p.acceptedLength.MinLength) - len(p.suffix)
									for {
										pos := bytes.Index(buffer[beforeSuffixLen:], p.suffix)
										if pos < 0 {
											p.streamOffset[dir] = len(buffers[dir])
											continue outer2
										}
										p.streamOffset[dir] += pos
										buffer = buffer[pos:]
										res = p.regex.FindSubmatchIndex(buffer[:p.acceptedLength.MinLength])
										if res != nil {
											break
										}
										p.streamOffset[dir]++
										buffer = buffer[1:]
									}
								} else {
									res = p.regex.FindSubmatchIndex(buffer)
								}

								if res == nil {
									p.streamOffset[dir] = len(buffers[dir])
									continue
								}
								if p.flags&progressVariantFlagState == progressVariantFlagStatePrecondition {
									recheckRegexes = true
									p.regex = nil
									p.flags += progressVariantFlagStatePreconditionMatched - progressVariantFlagStatePrecondition
									continue
								}
								p.nSuccessful++
								d := dcc.conditions[o.condition]
								if p.nSuccessful != len(d.Elements) {
									// remember that we advanced a sequence that has a follow up and we have to re-check the regexes
									recheckRegexes = true
								} else if d.Inverted {
									return false, nil
								}
								variableNames := p.regex.SubexpNames()
								p.regex = nil
								p.flags = 0
								for i := 2; i < len(res); i += 2 {
									varName := variableNames[i/2]
									if varName == "" {
										continue
									}
									if _, ok := p.variables[varName]; ok {
										return false, fmt.Errorf("variable %q already seen", varName)
									}
									if p.variables == nil {
										p.variables = make(map[string]string)
									}
									p.variables[varName] = string(buffer[res[i]:res[i+1]])
								}

								if res[1] != 0 {
									// update stream offsets: a follow up regex for the same direction
									// may consume the byte following the match, a regex for the other
									// direction may start reading from the next received packet,
									// so everything read before is out-of reach.
									p.streamOffset[dir] += res[1]
									for i := len(bufferLengths) - 1; ; i-- {
										if bufferLengths[i-1][dir] < p.streamOffset[dir] {
											p.streamOffset[(C2S^S2C)-dir] = bufferLengths[i][(C2S^S2C)-dir]
											break
										}
									}
								}
							}
						}
					}
					if !recheckRegexes {
						break
					}
				}

				// check if any of the regexe's failed and collect variable contents
				for cIdx, d := range dcc.conditions {
					pg := &progressGroups[cIdx]
					for pIdx := range pg.variants {
						p := &pg.variants[pIdx]
						nUnsuccessful := len(d.Elements) - p.nSuccessful
						if nUnsuccessful >= 2 || (nUnsuccessful != 0) != d.Inverted {
							if len(p.variant) == 0 {
								return false, nil
							}
							sqs := []string(nil)
							forbidden := []*bitmask.ConnectedBitmask(nil)
							for sq, v := range p.variant {
								sqs = append(sqs, sq)
								badSQR := &possibleSubQueries[sq].variableData[v].results
								forbidden = append(forbidden, badSQR)

							}
							sc.allowedSubQueries.remove(sqs, forbidden)
							if sc.allowedSubQueries.empty() {
								return false, nil
							}
							continue
						}
						if p.variables == nil {
							continue
						}
						if sc.outputVariables == nil {
							sc.outputVariables = make(map[string][]string)
						}
					outer:
						for n, v := range p.variables {
							values := sc.outputVariables[n]
							for _, on := range values {
								if n == on {
									continue outer
								}
							}
							sc.outputVariables[n] = append(values, v)
						}
					}
				}
				return true, nil
			}()
			// if it's a match on one of the converter outputs, there's no need to check the
			// other outputs.
			if ok {
				return true, nil
			}
			// if there's an error on any converter's output, always return it.
			if err != nil {
				return false, err
			}
		}
		return false, nil
	}), nil
}
