-- Forseti ttt plugin (Phase 2a + 2c)
-- Ties ttt into herdr + pi:
--   Forseti: Jump      (palette cmd) — pi wrote {path,line,end_line} to jump.json
--   Forseti: Review    (palette cmd) — pi wrote review.json (turn edit list)
--   forseti.ask        (ctrl+k a)    — fixed quick-ask with context
--   sidebar "Forseti"  — status + an input row: "ask pi about this …" with a
--                        real question; post-submit herdr agent focus ring.
--   context.json       — written on cursor/change/file events; the pi side
--                        reads it to become IDE-aware in every prompt.
-- Design: docs/design.md §2 · verified APIs: docs/spikes.md · plan: docs/implementation-plan.md
local ttt = require("ttt")
local json = require("ttt.json")
local fs = require("ttt.fs")
local sys = require("ttt.system")
local editor = require("ttt.editor")

local JUMP_FILE = ttt.plugin_dir() .. "/jump.json"
local CONTEXT_FILE = ttt.plugin_dir() .. "/context.json"
local REVIEW_FILE = ttt.plugin_dir() .. "/review.json"
local POLL_MS = 3000

local agents = {}          -- { {name, status, workspace_id}, ... }
local last_error = ""
local auto_focus = true    -- after ask, focus the pi pane to watch the answer
local last_ctx_write = 0

local function log_err(msg) ttt.log("error", "forseti: " .. msg) end

-- --- herdr plumbing --------------------------------------------------------

local function herdr_cmd(args)
  local ok, r = pcall(sys.exec, "herdr", args)
  if not ok or not r or not r.stdout or r.stdout == "" then return nil end
  return r.stdout
end

local function parse_agents(raw)
  local pi = {}
  -- kind-first (observed 0.9.3 key order); [^{}]- stays inside one flat object
  for kind, status, name, ws in raw:gmatch(
        '"agent":"([%w%-_]+)","agent_status":"([%w_]+)"[^{}]-"name":"([^"]+)"[^{}]-"workspace_id":"([^"]+)"') do
    if kind == "pi" then pi[#pi + 1] = { name = name, status = status, workspace_id = ws } end
  end
  if #pi == 0 then -- fallback: reversed order (future-proof)
    for name, status, ws in raw:gmatch(
          '"name":"([^"]+)"[^{}]-"agent_status":"([%w_]+)"[^{}]-"workspace_id":"([^"]+)"') do
      pi[#pi + 1] = { name = name, status = status or "unknown", workspace_id = ws }
    end
  end
  return pi
end

local function pi_agents()
  local out = herdr_cmd({ "agent", "list" })
  if not out then return {} end
  return parse_agents(out)
end

local function resolve_agent()
  agents = pi_agents()
  local mine = (pcall(sys.env, "HERDR_WORKSPACE_ID") and sys.env("HERDR_WORKSPACE_ID")) or ""
  if #agents == 0 then return nil, "no live pi agent — run forseti:open (herdr plugin) first" end
  local hits = {}
  for _, a in ipairs(agents) do
    if mine ~= "" and a.workspace_id == mine then hits[#hits + 1] = a end
  end
  if #hits == 1 then return hits[1] end
  if #hits > 1 then return nil, "ambiguous: " .. #hits .. " pi agents in this workspace" end
  if #agents == 1 then return agents[1] end
  return nil, #agents .. " pi agents live and none is in this workspace — ambiguous target"
end

-- --- editor context (what is the user looking at right now) ---------------

local function write_context()
  local ok_sel, path = pcall(editor.file_path)
  if not ok_sel or not path or path == "" then return end
  local now_secs = os.time()
  if now_secs - last_ctx_write < 1 then return end -- throttle storms
  last_ctx_write = now_secs
  local data = { path = path, ts = now_secs }
  local pos_ok, cur = pcall(editor.cursor)
  if pos_ok and cur then data.line, data.col = cur.line, cur.col end
  local sel_ok, sel = pcall(editor.selection)
  if sel_ok and sel and sel.active then
    data.sel_start, data.sel_end = sel.start_line, sel.end_line
    local text_ok, stext = pcall(editor.selection_text)
    if text_ok and stext then data.selection = stext end
  end
  local ok, encoded = pcall(json.encode, data)
  if ok then fs.write(CONTEXT_FILE, encoded) end
end

-- --- ask -------------------------------------------------------------------

local function clip(s, n)
  if #s <= n then return s end
  return s:sub(1, n) .. "\n… (clipped)"
end

local function editor_snippet()
  local path = (pcall(editor.file_path) and editor.file_path()) or "<unsaved buffer>"
  local cur = (pcall(editor.cursor) and editor.cursor()) or { line = 1, col = 1 }
  local sel = pcall(editor.selection) and editor.selection()
  local snippet, loc
  if sel and sel.active then
    snippet = editor.selection_text()
    loc = string.format("lines %d–%d", sel.start_line, sel.end_line)
  else
    snippet = editor.current_line()
    loc = "line " .. cur.line
  end
  return path, loc, clip(snippet or "", 3000)
end

local function submit_prompt(a, prompt, label)
  local out = herdr_cmd({ "agent", "prompt", a.name, prompt })
  if not out then
    last_error = "herdr agent prompt failed"
    log_err(last_error)
    return false
  end
  last_error = ""
  ttt.set_status_item("left", "ask", "asked pi (" .. a.name .. "): " .. label)
  ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
  if auto_focus then
    pcall(sys.exec, "herdr", { "agent", "focus", a.name })
  end
  return true
end

local function quick_ask()
  local ok, a_or_err = pcall(resolve_agent)
  if not ok or not a_or_err then last_error = tostring(a_or_err); log_err(last_error); return end
  local path, loc, snippet = editor_snippet()
  local prompt = string.format(
    'Look at %s (%s):\n"""\n%s\n"""\nExplain what this does and point out any problems, concisely. Do not edit anything yet.',
    path, loc, snippet)
  submit_prompt(a_or_err, prompt, "quick ask")
end

local function ask_with_question(question)
  local ok, a_or_err = pcall(resolve_agent)
  if not ok or not a_or_err then last_error = tostring(a_or_err); log_err(last_error); return end
  local path, loc, snippet = editor_snippet()
  local prompt = string.format(
    '[From ttt: %s (%s)]\n"""\n%s\n"""\n\n%s\n(Do not edit anything yet.)',
    path, loc, snippet, clip(question, 2000))
  submit_prompt(a_or_err, prompt, question)
end

-- --- Forseti: Jump / Review ------------------------------------------------

local function jump()
  local content = fs.read(JUMP_FILE)
  if not content then log_err("no jump request file at " .. JUMP_FILE) return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or not data.path then
    log_err("jump.json unreadable or missing 'path'"); return
  end
  local line = tonumber(data.line)
  if line and line > 0 then ttt.open_file(data.path, line)
  else ttt.open_file(data.path) end
  if line and tonumber(data.end_line) and data.end_line > line then
    editor.set_selection(line, 1, tonumber(data.end_line), 1)
  end
  last_error = ""
end

local function review()
  local content = fs.read(REVIEW_FILE)
  if not content or content == "" then log_err("no review summary"); return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or type(data.files) ~= "table" or #data.files == 0 then
    log_err("review.json has no files"); return
  end
  if ttt.open_tab then
    ttt.open_tab({
      title = "Forseti: Review",
      render = function(panel)
        panel:label("changes made by pi this turn:")
        panel:label("")
        for _, f in ipairs(data.files) do
          panel:label(string.format("%s :%d", f.path or "?", tonumber(f.line) or 1))
        end
        panel:label("")
        panel:label("(first change opened; use Forseti: Jump per file)")
      end,
    })
  end
  local first = data.files[1]
  ttt.open_file(first.path, tonumber(first.line) or 1)
end

-- --- Forseti: Review (turn summary from the pi side) ----------------------
local function review()
  local content = fs.read(REVIEW_FILE)
  if not content or content == "" then log_err("no review summary at " .. REVIEW_FILE) return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or type(data.files) ~= "table" or #data.files == 0 then
    log_err("review.json empty or malformed"); return
  end
  if ttt.open_tab then
    ttt.open_tab({
      title = "Forseti: Review",
      render = function(panel)
        panel:label("changes made by pi this turn:")
        panel:label("")
        for _, f in ipairs(data.files) do
          panel:label("- " .. (f.path or "?") .. " :" .. (tonumber(f.line) or 1))
        end
        panel:label("")
        panel:label("Forseti: Jump nudges you back to the first change")
      end,
    })
  end
  local first = data.files[1]
  ttt.open_file(first.path, tonumber(first.line) or 1)
end

-- --- sidebar ---------------------------------------------------------------

local glyph = { idle = "●", working = "◐", blocked = "!", done = "✓", unknown = "?" }

local function render(panel)
  if last_error ~= "" then
    panel:label("! " .. last_error)
    panel:label("")
  end
  panel:input({
    placeholder = "ask pi about this… (enter)",
    prefix = "",
    on_submit = function(text)
      if text ~= "" then ask_with_question(text) end
    end,
  })
  panel:label("")
  if #agents == 0 then
    panel:label("no pi agents live")
    panel:label("run: herdr plugin action")
    panel:label("     invoke forseti.open")
    return
  end
  for _, a in ipairs(agents) do
    local g = glyph[a.status] or "?"
    local txt = g .. " " .. a.name .. "  " .. a.status
    if a.status == "blocked" then txt = txt .. "  (approval UI)" end
    panel:label(txt)
  end
  panel:label("")
  panel:label("ctrl+k a  quick ask")
  panel:label("type above: custom ask")
end

ttt.register({
  sidebar = { title = "Forseti", render = render },
  commands = {
    { id = "forseti.jump", title = "Forseti: Jump", handler = jump },
    { id = "forseti.ask", title = "Forseti: Ask pi about selection", handler = quick_ask },
    { id = "forseti.review", title = "Forseti: Review", handler = review },
    {
      id = "forseti.toggle_focus",
      title = "Forseti: Toggle focus pi on ask",
      handler = function()
        auto_focus = not auto_focus
        ttt.set_status_item("left", "ask", "focus-on-ask " .. (auto_focus and "ON" or "OFF"))
        ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
      end,
    },
  },
  keybindings = {
    { key = "ctrl+k a", command = "forseti.ask" },
  },
})

ttt.set_interval(POLL_MS, function()
  local prev = agents
  agents = pi_agents()
  for _, a in ipairs(agents) do
    local was
    for _, p in ipairs(prev) do if p.name == a.name then was = p end end
    if was and was.status == "working" and a.status ~= "working" then
      if a.status == "blocked" then
        ttt.set_status_item("right", "agent-" .. a.name, "! " .. a.name .. " needs you")
      else
        ttt.set_status_item("right", "agent-" .. a.name, a.name .. " " .. (glyph[a.status] or ""))
      end
    end
  end
end)

-- IDE-awareness: keep context.json fresh for the pi side (Phase 2c-3)
local events_ok, events = pcall(require, "ttt.events")
if events_ok then
  events.on("cursor.change", function(_path) write_context() end)
  events.on("file.open", function(_path) write_context() end)
  events.on("file.save", function(_path) write_context() end)
else
  log_err("ttt.events not available; IDE context disabled")
end
