#!/usr/bin/env ruby

require "fileutils"
require "pathname"
require "find"

MAP_FILE, LOCAL_IGNORE, COMPAT_IGNORE = ".dot.map", ".dot-local-ignore", ".stow-local-ignore"
Mapping = Struct.new(:repo_rel, :system_raw, :system_abs, :line)

def clean(p); Pathname.new(p).cleanpath.to_s; end

class IgnoreMatcher
  attr_reader :basename, :path
  def initialize; @basename, @path = [], []; end

  def match(rel)
    rel = clean(rel)
    return false if rel == "." || rel.empty?

    base = File.basename(rel)
    return true if base == LOCAL_IGNORE || base == COMPAT_IGNORE

    @path.each { |re| rel.split("/").length.times { |i| s = rel.split("/")[i..].join("/"); return true if re.match?(s) || re.match?("/#{s}") } }
    @basename.any? { |re| re.match?(base) }
  end
end

def run(args)
  return print_usage if args.empty?
  dotfiles_dir = resolve_dotfiles_dir

  case args[0]
  when "track" then cmd_track(dotfiles_dir, args[1..])
  when "help", "-h", "--help" then print_usage
  else
    cmd = { "link" => :cmd_link, "list" => :cmd_list, "sync" => :cmd_sync }[args[0]]
    (print_usage; raise "unknown command: #{args[0]}") unless cmd
    raise "usage: dot #{args[0]}" if args.length != 1
    send(cmd, dotfiles_dir)
  end
end

def cmd_track(dotfiles_dir, args)
  raise "usage: dot track <file> [target_path]" unless (1..2).include?(args.length)
  source_abs = expand_path(args[0], dotfiles_dir)

  begin; info = File.lstat(source_abs); rescue Errno::ENOENT; raise "source file does not exist: #{source_abs}"; end
  raise "source path is a symlink (track expects a regular file or directory): #{source_abs}" if info.symlink?
  raise "source path must be a regular file or directory: #{source_abs}" unless info.file? || info.directory?

  repo_arg, repo_label = args.length == 2 ? [args[1], "target_path"] : [File.basename(source_abs), "source file name"]
  begin; repo_rel = sanitize_repo_path(repo_arg); rescue => e; raise "invalid #{repo_label}: #{e.message}"; end

  map_path = File.join(dotfiles_dir, MAP_FILE)
  mappings = parse_map(map_path, dotfiles_dir)
  mappings.each do |m|
    if m.system_abs == source_abs
      raise(m.repo_rel == repo_rel ? "already tracked: #{source_abs} -> #{repo_rel}" : "system path already tracked to #{m.repo_rel} (line #{m.line})")
    end
    raise "target_path already used by #{m.system_raw} (line #{m.line})" if m.repo_rel == repo_rel
  end

  repo_abs = File.join(dotfiles_dir, repo_rel)
  begin; s = File.lstat(repo_abs); raise(s.symlink? ? "target_path already exists as symlink: #{repo_abs}" : "target_path already exists: #{repo_abs}"); rescue Errno::ENOENT; end
  FileUtils.mkdir_p(File.dirname(repo_abs))

  begin; FileUtils.mv(source_abs, repo_abs); rescue => e; raise "move source into DOTFILES: #{e.message}"; end
  raise "move verification failed" unless File.exist?(repo_abs)

  begin; matcher = ignore_matcher_for_path(repo_abs, info); rescue => e; return rollback(repo_abs, source_abs, e, "load ignore file failed"); end

  begin
    matcher ? link_ignored_directory(repo_abs, source_abs, matcher) : File.symlink(repo_abs, source_abs)
  rescue => e; return rollback(repo_abs, source_abs, e, "create symlink failed") { FileUtils.rm_rf(source_abs) }; end

  begin; append_mapping(map_path, repo_rel, compress_home(source_abs))
  rescue => e; return rollback(repo_abs, source_abs, e, "write map failed") { FileUtils.rm_rf(source_abs) }; end

  puts "Tracked #{source_abs} -> #{repo_rel}"
end

def cmd_link(dotfiles_dir)
  with_mappings(dotfiles_dir) do |mappings|
    conflicts = mappings.sum { |m| link_mapping(dotfiles_dir, m) }
    $stderr.puts "Skipped #{conflicts} conflict(s)." if conflicts > 0
  end
end

def cmd_list(dotfiles_dir)
  with_mappings(dotfiles_dir) { |mappings| mappings.each { |m| puts "%-8s %s : %s" % [mapping_status(dotfiles_dir, m), m.repo_rel, m.system_raw] } }
end

def cmd_sync(dotfiles_dir)
  system("git", "add", ".", chdir: dotfiles_dir) || raise("git add failed")
  print "commit message: "
  msg = $stdin.gets&.strip
  raise "commit message cannot be empty" if msg.nil? || msg.empty?
  system("git", "commit", "-m", msg, chdir: dotfiles_dir) || raise("git commit failed")
  system("git", "push", chdir: dotfiles_dir) || raise("git push failed")
end

def with_mappings(dotfiles_dir)
  mappings = parse_map(File.join(dotfiles_dir, MAP_FILE), dotfiles_dir)
  mappings.empty? ? puts("No mappings found in #{File.join(dotfiles_dir, MAP_FILE)}") : yield(mappings)
end

def parse_map(map_path, dotfiles_dir)
  mappings = []
  visit_lines(map_path, true) do |n, line|
    line = line.strip
    next if line.empty? || line.start_with?("#")
    repo_part, system_part = line.split(":", 2)
    raise "invalid mapping at #{map_path}:#{n}" unless system_part
    mappings << Mapping.new(sanitize_repo_path(repo_part), system_part.strip, expand_path(system_part.strip, dotfiles_dir), n)
  end
  mappings
end

def append_mapping(map_path, repo_rel, system_path)
  FileUtils.mkdir_p(File.dirname(map_path))
  needs_nl = File.exist?(map_path) && (c = File.read(map_path); !c.empty? && !c.end_with?("\n"))
  File.open(map_path, "a") { |f| f.write("\n") if needs_nl; f.write("#{repo_rel} : #{system_path}\n") }
end

def mapping_status(dotfiles_dir, m)
  repo_abs = File.join(dotfiles_dir, m.repo_rel)
  matcher = ignore_matcher_for_path(repo_abs, nil)
  matcher ? status_with_ignore(repo_abs, m.system_abs, matcher) : inspect_link(m.system_abs, repo_abs)[1]
end

def symlink_points_to?(link, expected)
  target = File.readlink(link)
  target = File.join(File.dirname(link), target) unless Pathname.new(target).absolute?
  clean(target) == clean(expected)
rescue Errno::ENOENT, Errno::EINVAL; false; end

def link_mapping(dotfiles_dir, m)
  repo_abs = File.join(dotfiles_dir, m.repo_rel)
  matcher = ignore_matcher_for_path(repo_abs, nil)
  return link_ignored_directory(repo_abs, m.system_abs, matcher) if matcher

  exists, status = inspect_link(m.system_abs, repo_abs)
  return 0 if status == "OK"
  return ($stderr.puts "Warning: conflict at #{m.system_abs} (manual resolution required)"; 1) if exists

  FileUtils.mkdir_p(File.dirname(m.system_abs))
  File.symlink(repo_abs, m.system_abs)
  puts "Linked #{m.system_abs} -> #{repo_abs}"
  0
end

def link_ignored_directory(repo_dir, system_dir, matcher)
  conflicts, ready = ensure_managed(repo_dir, system_dir, true)
  return conflicts unless ready

  walk_managed(repo_dir, system_dir, matcher) do |rp, sp, dir|
    c, r = ensure_managed(rp, sp, dir)
    conflicts += c
    :skip_dir if dir && !r
  end

  puts "Linked #{system_dir} -> #{repo_dir} (ignoring matched entries)"
  conflicts
end

def ensure_managed(repo_path, system_path, dir)
  unless dir
    exists, status = inspect_link(system_path, repo_path)
    return [0, true] if status == "OK"
    return (File.symlink(repo_path, system_path); [0, true]) unless exists
    return ($stderr.puts "Warning: conflict at #{system_path} (manual resolution required)"; [1, false])
  end

  begin
    info = File.lstat(system_path)
    if info.directory? then [0, true]
    elsif info.symlink? && symlink_points_to?(system_path, repo_path)
      File.delete(system_path); FileUtils.mkdir_p(system_path); [0, true]
    else; $stderr.puts "Warning: conflict at #{system_path} (manual resolution required)"; [1, false]; end
  rescue Errno::ENOENT; FileUtils.mkdir_p(system_path); [0, true]; end
end

def ignore_matcher_for_path(path, info)
  info ||= (begin; File.lstat(path); rescue Errno::ENOENT; return nil; end)
  return nil if info.symlink? || !info.directory?
  load_ignore_matcher(path)
end

def load_ignore_matcher(dir)
  matcher, loaded = IgnoreMatcher.new, false
  [LOCAL_IGNORE, COMPAT_IGNORE].each do |name|
    found = visit_lines(File.join(dir, name), true) do |n, line|
      pat = strip_comment(line)
      next if pat.empty?
      (pat.include?("/") ? matcher.path : matcher.basename) << Regexp.new("^(?:#{pat})$")
    end
    loaded ||= found
  end
  loaded ? matcher : nil
end

def visit_lines(path, allow_missing)
  return false unless File.exist?(path)
  File.readlines(path, chomp: true).each_with_index { |l, i| yield(i + 1, l) }
  true
rescue Errno::ENOENT; raise unless allow_missing; false; end

def strip_comment(line)
  line.length.times do |i|
    next unless line[i] == "#"
    bs = 0; (i - 1).downto(0) { |j| break unless line[j] == "\\"; bs += 1 }
    return line[0...i].strip if bs.even?
  end
  line.strip
end

def status_with_ignore(repo_dir, system_dir, matcher)
  return "BROKEN" unless File.exist?(repo_dir)
  begin; info = File.lstat(system_dir); rescue Errno::ENOENT; return "MISSING"; end
  return "STRAY" unless info.directory?

  status = "OK"
  catch(:done) do
    walk_managed(repo_dir, system_dir, matcher) do |rp, sp, dir|
      begin; si = File.lstat(sp); rescue Errno::ENOENT; status = "MISSING"; throw :done; end
      if dir; (status = "STRAY"; throw :done) unless si.directory?; next; end
      _, st = inspect_link(sp, rp)
      (status = st; throw :done) if st != "OK"
    end
  end
  status
end

def walk_managed(repo_dir, system_dir, matcher)
  Find.find(repo_dir) do |rp|
    next if rp == repo_dir
    rel = Pathname.new(rp).relative_path_from(Pathname.new(repo_dir)).to_s
    if matcher.match(rel); Find.prune if File.directory?(rp) && !File.symlink?(rp); next; end
    dir = File.directory?(rp) && !File.symlink?(rp)
    result = yield(rp, File.join(system_dir, rel), dir)
    Find.prune if result == :skip_dir && dir
  end
end

def inspect_link(system_path, repo_path)
  begin; info = File.lstat(system_path); rescue Errno::ENOENT; return [false, "MISSING"]; end
  return [true, "STRAY"] unless info.symlink?
  return [true, "STRAY"] unless symlink_points_to?(system_path, repo_path)
  return [true, "BROKEN"] unless File.exist?(repo_path)
  [true, "OK"]
end

def rollback(repo_abs, source_abs, cause, action, &cleanup)
  cleanup&.call
  begin; FileUtils.mv(repo_abs, source_abs); rescue => e
    raise "#{action} (#{cause.message}), cleanup succeeded, rollback failed (#{e.message})"
  end
  raise "#{action}: #{cause.message}"
end

def resolve_dotfiles_dir
  raw = ENV.fetch("DOTFILES", "").strip
  expand_path(raw.empty? ? "~/.dotfiles" : raw, "")
end

def expand_path(raw, dotfiles_dir)
  p = raw.strip; raise "empty path" if p.empty?
  home = Dir.home

  if p == "~" then p = home
  elsif p.start_with?("~/") then p = File.join(home, p[2..])
  elsif p.start_with?("~") then raise "unsupported home expansion form: #{raw}"; end

  p = p.gsub(/\$([A-Z_]+)/) { |m| k = $1; k == "DOTFILES" && !dotfiles_dir.empty? ? dotfiles_dir : ENV.fetch(k, "") }
  p = File.expand_path(p) unless Pathname.new(p).absolute?
  clean(p)
end

def sanitize_repo_path(raw)
  c = clean(raw.strip)
  raise "path cannot be empty" if c.empty?
  raise "path must be inside DOTFILES: #{raw}" if c == "." || c == ".." || c.start_with?("../")
  raise "path must be relative: #{raw}" if c.start_with?("/")
  c
end

def compress_home(path)
  home = clean(Dir.home); path = clean(path)
  return "~" if path == home
  path.start_with?(home + "/") ? "~#{path[home.length..]}" : path
end

def print_usage
  puts "dot - minimalist dotfile manager\n\nusage:\n  dot track <file> [target_path]\n  dot link\n  dot list\n  dot sync\n\nEnvironment:\n  DOTFILES  Repository path (default: ~/.dotfiles)\n"
end

if __FILE__ == $PROGRAM_NAME
  begin; run(ARGV); rescue => e; $stderr.puts "dot: #{e.message}"; exit 1; end
end
