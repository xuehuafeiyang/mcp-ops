package builtins

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"mcp-ops/internal/mcp"
)

// RegisterMemory 注册官方 server-memory 的持久化知识图谱工具。
// 数据模型：实体（name/entityType/observations）+ 关系（from/to/relationType）。
// 存储为 JSON 文件，跨会话保留。
func RegisterMemory(s *mcp.Server, storagePath string) {
	store := newMemoryStore(storagePath)

	s.RegisterTool("create_entities",
		"创建新实体（知识图谱节点），每个实体可带类型与观察项列表。",
		schema(`"entities":{"type":"array","items":{"type":"object","properties":{
			"name":{"type":"string"},"entityType":{"type":"string"},"observations":{"type":"array","items":{"type":"string"}}},
			"required":["name"]}}`, "entities"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			ents := parseEntities(args["entities"])
			if len(ents) == 0 {
				return errR("entities 不能为空")
			}
			return mcp.TextResult(store.createEntities(ents))
		})

	s.RegisterTool("create_relations",
		"在实体之间创建关系（from → to，带关系类型）。",
		schema(`"relations":{"type":"array","items":{"type":"object","properties":{
			"from":{"type":"string"},"to":{"type":"string"},"relationType":{"type":"string"}},
			"required":["from","to","relationType"]}}`, "relations"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			rels := parseRelations(args["relations"])
			if len(rels) == 0 {
				return errR("relations 不能为空")
			}
			return mcp.TextResult(store.createRelations(rels))
		})

	s.RegisterTool("add_observations",
		"给已有实体追加观察项（新的事实/属性）。",
		schema(`"observations":{"type":"array","items":{"type":"object","properties":{
			"entityName":{"type":"string"},"contents":{"type":"array","items":{"type":"string"}}},
			"required":["entityName","contents"]}}`, "observations"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			var added []string
			switch v := args["observations"].(type) {
			case []any:
				for _, e := range v {
					if m, ok := e.(map[string]any); ok {
						name := argString(m, "entityName")
						for _, c := range argStrings(m, "contents") {
							if store.addObservation(name, c) {
								added = append(added, name+": "+c)
							}
						}
					}
				}
			}
			if len(added) == 0 {
				return errR("没有可添加的观察项（或实体不存在）")
			}
			return mcp.TextResult(store.persist("已添加 " + strings.Join(added, "; ")))
		})

	s.RegisterTool("delete_entities",
		"删除实体（同时删除指向它们的关系）。",
		schema(`"entityNames":{"type":"array","items":{"type":"string"}}`, "entityNames"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			names := argStrings(args, "entityNames")
			return mcp.TextResult(store.deleteEntities(names))
		})

	s.RegisterTool("delete_observations",
		"删除实体的指定观察项。",
		schema(`"deletions":{"type":"array","items":{"type":"object","properties":{
			"entityName":{"type":"string"},"observations":{"type":"array","items":{"type":"string"}}}}}`, "deletions"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			var removed int
			switch v := args["deletions"].(type) {
			case []any:
				for _, e := range v {
					if m, ok := e.(map[string]any); ok {
						name := argString(m, "entityName")
						for _, obs := range argStrings(m, "observations") {
							if store.deleteObservation(name, obs) {
								removed++
							}
						}
					}
				}
			}
			return mcp.TextResult(store.persist(fmt.Sprintf("已删除 %d 条观察项", removed)))
		})

	s.RegisterTool("delete_relations",
		"删除指定关系。",
		schema(`"relations":{"type":"array","items":{"type":"object","properties":{
			"from":{"type":"string"},"to":{"type":"string"},"relationType":{"type":"string"}},
			"required":["from","to","relationType"]}}`, "relations"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			rels := parseRelations(args["relations"])
			return mcp.TextResult(store.deleteRelations(rels))
		})

	s.RegisterTool("read_graph",
		"读取整个知识图谱（所有实体与关系）。",
		schema(``),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			return mcp.TextResult(store.readGraph())
		})

	s.RegisterTool("search_nodes",
		"按名称/类型/观察项关键词模糊搜索实体。",
		schema(`"query":{"type":"string"}`, "query"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			q := strings.ToLower(strings.TrimSpace(argString(args, "query")))
			if q == "" {
				return errR("query 不能为空")
			}
			return mcp.TextResult(store.searchNodes(q))
		})

	s.RegisterTool("open_nodes",
		"读取指定实体的详情（含观察项与关联关系）。",
		schema(`"names":{"type":"array","items":{"type":"string"}}`, "names"),
		func(ctx mcp.CallContext, args map[string]any) *mcp.CallToolResult {
			names := argStrings(args, "names")
			if len(names) == 0 {
				return errR("names 不能为空")
			}
			return mcp.TextResult(store.openNodes(names))
		})
}

type entity struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

type relation struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

type graphData struct {
	Entities  []entity   `json:"entities"`
	Relations []relation `json:"relations"`
}

type memoryStore struct {
	mu        sync.Mutex
	path      string
	entities  map[string]*entity
	relations []relation
}

func newMemoryStore(path string) *memoryStore {
	s := &memoryStore{path: path, entities: map[string]*entity{}}
	if data, err := os.ReadFile(path); err == nil {
		var g graphData
		if json.Unmarshal(data, &g) == nil {
			for i := range g.Entities {
				s.entities[g.Entities[i].Name] = &g.Entities[i]
			}
			s.relations = g.Relations
		}
	}
	return s
}

// persist 保存到磁盘并返回带状态的前缀消息。
func (s *memoryStore) persist(msg string) string {
	g := graphData{}
	for _, e := range s.entities {
		g.Entities = append(g.Entities, *e)
	}
	sort.Slice(g.Entities, func(i, j int) bool { return g.Entities[i].Name < g.Entities[j].Name })
	g.Relations = s.relations
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return msg + "\n(保存失败: " + err.Error() + ")"
	}
	if err := os.WriteFile(s.path, data, 0o644); err != nil {
		return msg + "\n(保存失败: " + err.Error() + ")"
	}
	return msg + fmt.Sprintf("\n(已持久化到 %s，实体 %d 个，关系 %d 条)", s.path, len(s.entities), len(s.relations))
}

func (s *memoryStore) createEntities(ents []entity) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var created []string
	for _, e := range ents {
		if e.Name == "" {
			continue
		}
		if existing, ok := s.entities[e.Name]; ok {
			// 已存在：合并观察项
			seen := map[string]bool{}
			for _, o := range existing.Observations {
				seen[o] = true
			}
			for _, o := range e.Observations {
				if !seen[o] {
					existing.Observations = append(existing.Observations, o)
				}
			}
			created = append(created, e.Name+"（已存在，合并观察项）")
			continue
		}
		e.Observations = append([]string{}, e.Observations...)
		s.entities[e.Name] = &e
		created = append(created, e.Name)
	}
	return s.persist("已创建/更新实体：" + strings.Join(created, ", "))
}

func (s *memoryStore) createRelations(rels []relation) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var created []string
	for _, r := range rels {
		if _, ok := s.entities[r.From]; !ok {
			return fmt.Sprintf("错误：实体 %q 不存在，请先 create_entities", r.From)
		}
		if _, ok := s.entities[r.To]; !ok {
			return fmt.Sprintf("错误：实体 %q 不存在，请先 create_entities", r.To)
		}
		if !hasRelation(s.relations, r) {
			s.relations = append(s.relations, r)
		}
		created = append(created, r.From+" -["+r.RelationType+"]-> "+r.To)
	}
	return s.persist("已创建关系：" + strings.Join(created, "; "))
}

func (s *memoryStore) addObservation(name, content string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entities[name]
	if !ok || content == "" {
		return false
	}
	for _, o := range e.Observations {
		if o == content {
			return false
		}
	}
	e.Observations = append(e.Observations, content)
	return true
}

func (s *memoryStore) deleteEntities(names []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []string
	for _, n := range names {
		if _, ok := s.entities[n]; ok {
			delete(s.entities, n)
			removed = append(removed, n)
		}
	}
	// 清理关联关系
	kept := s.relations[:0]
	for _, r := range s.relations {
		if !contains(names, r.From) && !contains(names, r.To) {
			kept = append(kept, r)
		}
	}
	s.relations = kept
	return s.persist("已删除实体：" + strings.Join(removed, ", "))
}

func (s *memoryStore) deleteObservation(name, obs string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entities[name]
	if !ok {
		return false
	}
	for i, o := range e.Observations {
		if o == obs {
			e.Observations = append(e.Observations[:i], e.Observations[i+1:]...)
			return true
		}
	}
	return false
}

func (s *memoryStore) deleteRelations(rels []relation) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed int
	kept := s.relations[:0]
	for _, r := range s.relations {
		if hasRelation(rels, r) {
			removed++
		} else {
			kept = append(kept, r)
		}
	}
	s.relations = kept
	return s.persist(fmt.Sprintf("已删除 %d 条关系", removed))
}

func (s *memoryStore) readGraph() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString("知识图谱：\n\n实体（")
	b.WriteString(fmt.Sprintf("%d 个）：\n", len(s.entities)))
	names := make([]string, 0, len(s.entities))
	for n := range s.entities {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := s.entities[n]
		fmt.Fprintf(&b, "- %s [%s]", e.Name, e.EntityType)
		if len(e.Observations) > 0 {
			b.WriteString(": " + strings.Join(e.Observations, "; "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n关系（")
	b.WriteString(fmt.Sprintf("%d 条）：\n", len(s.relations)))
	for _, r := range s.relations {
		fmt.Fprintf(&b, "- %s -[%s]-> %s\n", r.From, r.RelationType, r.To)
	}
	if len(s.entities) == 0 && len(s.relations) == 0 {
		b.WriteString("（空图谱，可用 create_entities 开始构建）")
	}
	return b.String()
}

func (s *memoryStore) searchNodes(q string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var hits []string
	for name, e := range s.entities {
		if strings.Contains(strings.ToLower(name), q) ||
			strings.Contains(strings.ToLower(e.EntityType), q) {
			hits = append(hits, name)
			continue
		}
		for _, o := range e.Observations {
			if strings.Contains(strings.ToLower(o), q) {
				hits = append(hits, name)
				break
			}
		}
	}
	sort.Strings(hits)
	if len(hits) == 0 {
		return fmt.Sprintf("未找到匹配 %q 的实体", q)
	}
	return fmt.Sprintf("匹配 %q 的实体 %d 个：\n- %s", q, len(hits), strings.Join(hits, "\n- "))
}

func (s *memoryStore) openNodes(names []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	for _, n := range names {
		e, ok := s.entities[n]
		if !ok {
			fmt.Fprintf(&b, "=== %s ===\n(不存在)\n\n", n)
			continue
		}
		fmt.Fprintf(&b, "=== %s ===\n类型: %s\n观察项:\n", e.Name, e.EntityType)
		if len(e.Observations) == 0 {
			b.WriteString("  (无)\n")
		} else {
			for _, o := range e.Observations {
				b.WriteString("  - " + o + "\n")
			}
		}
		b.WriteString("关联关系:\n")
		found := false
		for _, r := range s.relations {
			if r.From == n {
				fmt.Fprintf(&b, "  -[%s]-> %s\n", r.RelationType, r.To)
				found = true
			}
			if r.To == n {
				fmt.Fprintf(&b, "  %s -[%s]-> (本实体)\n", r.From, r.RelationType)
				found = true
			}
		}
		if !found {
			b.WriteString("  (无)\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func parseEntities(v any) []entity {
	var out []entity
	switch arr := v.(type) {
	case []any:
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				out = append(out, entity{
					Name:         argString(m, "name"),
					EntityType:   argString(m, "entityType"),
					Observations: argStrings(m, "observations"),
				})
			}
		}
	case []map[string]any:
		for _, m := range arr {
			out = append(out, entity{
				Name:         argString(m, "name"),
				EntityType:   argString(m, "entityType"),
				Observations: argStrings(m, "observations"),
			})
		}
	}
	return out
}

func parseRelations(v any) []relation {
	var out []relation
	switch arr := v.(type) {
	case []any:
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				out = append(out, relation{From: argString(m, "from"), To: argString(m, "to"), RelationType: argString(m, "relationType")})
			}
		}
	case []map[string]any:
		for _, m := range arr {
			out = append(out, relation{From: argString(m, "from"), To: argString(m, "to"), RelationType: argString(m, "relationType")})
		}
	}
	return out
}

func hasRelation(list []relation, r relation) bool {
	for _, x := range list {
		if x.From == r.From && x.To == r.To && x.RelationType == r.RelationType {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
