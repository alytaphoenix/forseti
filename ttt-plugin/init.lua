-- Forseti ttt plugin (Phase 2a)
-- Ties ttt into herdr + pi:
--   Forseti: Jump    palette command — pi wrote {path,line,end_line} to jump.json
--                    (in THIS plugin dir; /tmp is outside the fs sandbox); we open
--                    the file at the line and highlight the changed range.
--   forseti.ask      ctrl+k a — send the selection (or cursor line) to the live
--                    pi agent via `herdr agent prompt` (herdr tracks its lifecycle).
--   sidebar "Forseti" — poll `herdr agent list` every 3 s; render pi agents +
--                    lifecycle state (idle|working|blocked|done|unknown).
-- Design: docs/design.md §2 · verified APIs: docs/spikes.md
local ttt = require("ttt")
local json = require("ttt.json")
local fs = require("ttt.fs")
local sys = require("ttt.system")
local editor = require("ttt.editor")

local JUMP_FILE = ttt.plugin_dir() .. "/jump.json"
local POLL_MS = 3000

-- state for the sidebar render
local agents = {}            -- { {name, status, workspace_id}, ... }
local last_error = ""

local function log_err(msg) ttt.log("error", "forseti: " .. msg) end
local notified_done = {}   -- agent name -> true after we've pinged a settled state

-- --- agent discovery -----------------------------------------------------
-- herdr agent list emits flat JSON objects; per observed 0.9.3 output the key
-- order is  "agent":"<kind>","agent_status":"<state>" ... "name":"<n>" ...
-- "workspace_id":"<w5>" — inside ONE flat object. The [^{}]- spans same-object
-- content only. Fallback tries the reverse order in case versions reorder.
local function parse_agents(raw)
  local found = {}
  for status, name, ws in raw:gmatch('"agent_status":"([%w_]+)"[^{}]-"name":"([^"]+)"[^{}]-"workspace_id":"([^"]+)"') do
    found[#found + 1] = { name = name, status = status, workspace_id = ws }
  end
  if #found == 0 then
    for name, status, ws in raw:gmatch('"name":"([^"]+)"[^{}]-"agent_status":"([%w_]+)"[^{}]-"workspace_id":"([^"]+)"') do
      found[#found + 1] = { name = name, status = status, workspace_id = ws }
    end
  end
  local pi = {}
  for _, a in ipairs(found) do
    if a.name and raw:match('"agent":"pi"') then pi[#pi + 1] = a end
  end
  -- naive kind filter above keeps every flat object; re-filter by proximity:
  if #pi == 0 then
    for status, name, ws, kind in raw:gmatch('"agent":"([%w%-_]+)","agent_status":"([%w_]+)"[^{}]-"name":"([^"]+)"[^{}]-"workspace_id":"([^"]+)"') do
      if kind == "pi" then pi[#pi + 1] = { name = name, status = status, workspace_id = ws } end
    end
  end
  return pi
end

local function pi_agents()
  local ok, r = pcall(sys.exec, "herdr", { "agent", "list" })
  if not ok or not r or not r.stdout or r.stdout == "" then return {} end
  return parse_agents(r.stdout)
end

--- resolve the agent to prompt: same workspace first (HERDR_* present when ttt
--- was launched from a herdr pane), else any single pi agent, else nil, err.
local function resolve_agent()
  agents = pi_agents()
  local mine = sys.env("HERDR_WORKSPACE_ID") or ""
  if #agents == 0 then return nil, "no live pi agent — run forseti:open (herdr plugin) first" end
  local hits = {}
  for _, a in ipairs(agents) do
    if mine ~= "" and a.workspace_id == mine then hits[#hits + 1] = a end
  end
  if #hits == 1 then return hits[1] end
  if #hits > 1 then return nil, "ambiguous: " .. #hits .. " pi agents in this workspace — name one via FORSETI_AGENT in herdr config" end
  if #agents == 1 then return agents[1] end
  return nil, #agents .. " pi agents live and none is in this workspace — ambiguous target"
end

-- --- forseti.ask ---------------------------------------------------------
local function clip(s, n)
  if #s <= n then return s end
  return s:sub(1, n) .. "\n… (clipped)"
end

local function ask()
  local ok, a_or_err = pcall(resolve_agent)
  if not ok or not a_or_err then
    last_error = tostring(a_or_err); log_err(last_error); return
  end
  local a = a_or_err
  local path = editor.file_path() or "<unsaved buffer>"
  local cur = editor.cursor() or { line = 1, col = 1 }
  local sel = editor.selection()
  local snippet, loc
  if sel and sel.active then
    snippet = editor.selection_text()
    loc = string.format("lines %d–%d", sel.start_line, sel.end_line)
  else
    snippet = editor.current_line()
    loc = "line " .. cur.line
  end
  local prompt = string.format(
    'Look at %s (%s):\n"""\n%s\n"""\nExplain what this does and point out any problems, concisely. Do not edit anything yet.',
    path, loc, clip(snippet, 3000))
  local ok2, r = pcall(sys.exec, "herdr", { "agent", "prompt", a.name, prompt })
  if not ok2 or not r or not r.stdout or r.stdout == "" then
    last_error = "herdr agent prompt failed (rc=" .. tostring(r and r.exit_code) .. ")"
    log_err(last_error); return
  end
  last_error = ""
  -- no --wait: submission is quick; the sidebar shows working→done transitions.
  ttt.set_status_item("left", "ask", "asked pi (" .. a.name .. ")")
  ttt.set_timeout(2500, function() ttt.remove_status_item("ask") end)
end

-- --- Forseti: Jump -------------------------------------------------------
local function jump()
  local content = fs.read(JUMP_FILE)
  if not content then log_err("no jump request file at " .. JUMP_FILE) return end
  local ok, data = pcall(json.decode, content)
  if not ok or type(data) ~= "table" or not data.path then
    log_err("jump.json unreadable or missing 'path'"); return
  end
  local line = tonumber(data.line)
  if line and line > 0 then
    ttt.open_file(data.path, line)
  else
    ttt.open_file(data.path)
  end
  if line and tonumber(data.end_line) and data.end_line > line then
    -- highlight the changed hunk pi just touched
    editor.set_selection(line, 1, tonumber(data.end_line), 1)
  end
  last_error = ""
end

-- --- sidebar -------------------------------------------------------------
local glyph = {
  idle = "●", working = "◐", blocked = "!", done = "✓", unknown = "?",
}

local function render(panel)
  if last_error ~= "" then
    panel:label("! " .. last_error)
    panel:label("")
  end
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
  panel:label("ctrl+k a  ask pi")
end

ttt.register({
  sidebar = { title = "Forseti", render = render },
  commands = {
    { id = "forseti.jump", title = "Forseti: Jump", handler = jump },
    { id = "forseti.ask", title = "Forseti: Ask pi about selection", handler = ask },
  },
  keybindings = {
    { key = "ctrl+k a", command = "forseti.ask" },
  },
})

ttt.set_interval(POLL_MS, function()
  local prev = agents
  agents = pi_agents()
  -- notify on working -> settled transitions (done shown by herdr's own badge
  -- distinction; we only ping once per settled state to stay quiet)
  for _, a in ipairs(agents) do
    local was = nil
    for _, p in ipairs(prev) do if p.name == a.name then was = p end end
    if was and was.status == "working" and a.status ~= "working" then
      notified_done[a.name] = nil
      if a.status == "blocked" then
        ttt.set_status_item("right", "agent-" .. a.name, "! " .. a.name .. " needs you")
      else
        ttt.set_status_item("right", "agent-" .. a.name, a.name .. " " .. (glyph[a.status] or a.status))
      end
    end
  end
end)
