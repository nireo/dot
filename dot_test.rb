require "minitest/autorun"
require "tmpdir"
require "fileutils"
require_relative "dot"

class TestRepoPathForTrackTarget < Minitest::Test
  def test_exact_target_path
    assert_equal "pi", repo_path_for_track_target("pi", "/tmp/home/.pi/agents")
  end

  def test_trailing_slash_appends_source_basename
    assert_equal "pi/agents", repo_path_for_track_target("pi/", "/tmp/home/.pi/agents")
  end

  def test_trailing_slash_works_for_files
    assert_equal "shell/.bashrc", repo_path_for_track_target("shell/", "/tmp/home/.bashrc")
  end
end

class TestSanitizeRepoPath < Minitest::Test
  def test_simple
    assert_equal "shell/.zshrc", sanitize_repo_path("shell/.zshrc")
  end

  def test_trims_whitespace
    assert_equal "nvim/init.lua", sanitize_repo_path("  nvim/init.lua  ")
  end

  def test_normalizes_dots
    assert_equal "a/c", sanitize_repo_path("a/./b/../c")
  end

  def test_rejects_empty
    assert_raises(RuntimeError) { sanitize_repo_path("") }
  end

  def test_rejects_parent
    assert_raises(RuntimeError) { sanitize_repo_path("../x") }
  end

  def test_rejects_absolute
    assert_raises(RuntimeError) { sanitize_repo_path("/etc/passwd") }
  end
end

class TestExpandPath < Minitest::Test
  def test_expands_tilde
    home = Dir.home
    got = expand_path("~/.config", "")
    assert_equal File.join(home, ".config"), got
  end

  def test_expands_dotfiles_override
    Dir.mktmpdir do |tmp|
      dotfiles = File.join(tmp, "mydotfiles")
      got = expand_path("$DOTFILES/shell/.zshrc", dotfiles)
      assert_equal File.join(dotfiles, "shell", ".zshrc"), got
    end
  end

  def test_rejects_unsupported_tilde_form
    assert_raises(RuntimeError) { expand_path("~other/file", "") }
  end
end

class TestParseMap < Minitest::Test
  def test_parses_valid_map
    Dir.mktmpdir do |dotfiles|
      map_path = File.join(dotfiles, MAP_FILE)
      content = [
        "# shell",
        "",
        "shell/.zshrc : ~/.zshrc",
        "nvim/init.lua : ~/.config/nvim/init.lua",
      ].join("\n") + "\n"

      File.write(map_path, content)

      mappings = parse_map(map_path, dotfiles)
      assert_equal 2, mappings.length
      assert_equal "shell/.zshrc", mappings[0].repo_rel
      assert_equal "~/.zshrc", mappings[0].system_raw

      home = Dir.home
      assert_equal File.join(home, ".zshrc"), mappings[0].system_abs
    end
  end

  def test_invalid_line_returns_error
    Dir.mktmpdir do |dotfiles|
      bad_path = File.join(dotfiles, "bad.map")
      File.write(bad_path, "invalid-line\n")

      assert_raises(RuntimeError) { parse_map(bad_path, dotfiles) }
    end
  end
end

class TestIgnoreMatcherMatch < Minitest::Test
  def test_match
    Dir.mktmpdir do |package_dir|
      ignore_path = File.join(package_dir, COMPAT_IGNORE)
      content = [
        "# comments are ignored",
        "README.*",
        "^/docs/.*",
        'foo\#bar # keep escaped hash',
      ].join("\n") + "\n"

      File.write(ignore_path, content)

      matcher = load_ignore_matcher(package_dir)
      refute_nil matcher

      assert matcher.match("README.md")
      assert matcher.match("docs/init.lua")
      assert matcher.match("foo#bar")
      assert matcher.match(LOCAL_IGNORE)
      assert matcher.match(COMPAT_IGNORE)
      refute matcher.match("lua/plugins.lua")
    end
  end
end

class TestSymlinkPointsTo < Minitest::Test
  def test_points_to
    Dir.mktmpdir do |dir|
      target = File.join(dir, "target.txt")
      File.write(target, "x")

      link = File.join(dir, "link")
      File.symlink("target.txt", link)

      assert symlink_points_to?(link, target)

      other = File.join(dir, "other.txt")
      File.write(other, "y")

      refute symlink_points_to?(link, other)
    end
  end
end

class TestMappingStatus < Minitest::Test
  def test_missing
    Dir.mktmpdir do |dotfiles|
      system_path = File.join(dotfiles, "home", ".zshrc")
      m = Mapping.new("shell/.zshrc", system_path, system_path, 1)

      assert_equal "MISSING", mapping_status(dotfiles, m)
    end
  end

  def test_stray_regular_file
    Dir.mktmpdir do |dotfiles|
      system_path = File.join(dotfiles, "home", ".zshrc")
      FileUtils.mkdir_p(File.dirname(system_path))
      File.write(system_path, "stray")

      m = Mapping.new("shell/.zshrc", system_path, system_path, 1)
      assert_equal "STRAY", mapping_status(dotfiles, m)
    end
  end

  def test_broken
    Dir.mktmpdir do |dotfiles|
      repo_rel = "shell/.zshrc"
      repo_abs = File.join(dotfiles, repo_rel)
      system_path = File.join(dotfiles, "home", ".zshrc")

      FileUtils.mkdir_p(File.dirname(system_path))
      File.symlink(repo_abs, system_path)

      m = Mapping.new(repo_rel, system_path, system_path, 1)
      assert_equal "BROKEN", mapping_status(dotfiles, m)
    end
  end

  def test_ok
    Dir.mktmpdir do |dotfiles|
      repo_rel = "shell/.zshrc"
      repo_abs = File.join(dotfiles, repo_rel)
      system_path = File.join(dotfiles, "home", ".zshrc")

      FileUtils.mkdir_p(File.dirname(repo_abs))
      File.write(repo_abs, "ok")

      FileUtils.mkdir_p(File.dirname(system_path))
      File.symlink(repo_abs, system_path)

      m = Mapping.new(repo_rel, system_path, system_path, 1)
      assert_equal "OK", mapping_status(dotfiles, m)
    end
  end
end

class TestAppendMapping < Minitest::Test
  def test_append
    Dir.mktmpdir do |dir|
      map_path = File.join(dir, MAP_FILE)

      append_mapping(map_path, "shell/.zshrc", "~/.zshrc")
      append_mapping(map_path, "nvim/init.lua", "~/.config/nvim/init.lua")

      text = File.read(map_path)
      assert_includes text, "shell/.zshrc : ~/.zshrc\n"
      assert_includes text, "nvim/init.lua : ~/.config/nvim/init.lua\n"
    end
  end
end

class TestIntegrationTrackAndLink < Minitest::Test
  def test_track_and_link
    Dir.mktmpdir do |root|
      dotfiles_dir = File.join(root, "dotfiles")
      home_dir = File.join(root, "home")
      system_file = File.join(home_dir, ".bashrc")

      FileUtils.mkdir_p(home_dir)

      original_content = "export TEST_VAR=1\n"
      File.write(system_file, original_content)

      cmd_track(dotfiles_dir, [system_file, "shell/.bashrc"])

      repo_file = File.join(dotfiles_dir, "shell", ".bashrc")

      assert_equal original_content, File.read(repo_file)
      assert symlink_points_to?(system_file, repo_file)

      File.delete(system_file)

      cmd_link(dotfiles_dir)

      assert symlink_points_to?(system_file, repo_file)

      map_path = File.join(dotfiles_dir, MAP_FILE)
      map_data = File.read(map_path)
      assert_includes map_data, "shell/.bashrc : "
    end
  end
end

class TestIntegrationUntrack < Minitest::Test
  def test_untrack_file_restores_original_and_preserves_other_mappings
    Dir.mktmpdir do |root|
      dotfiles = File.join(root, "dotfiles")
      home = File.join(root, "home")
      FileUtils.mkdir_p(home)
      first = File.join(home, ".bashrc")
      second = File.join(home, ".zshrc")
      File.write(first, "bash settings\n")
      File.write(second, "zsh settings\n")
      cmd_track(dotfiles, [first, "shell/.bashrc"])
      cmd_track(dotfiles, [second, "shell/.zshrc"])

      cmd_untrack(dotfiles, [first])

      assert_equal "bash settings\n", File.read(first)
      refute File.symlink?(first)
      refute File.exist?(File.join(dotfiles, "shell", ".bashrc"))
      assert symlink_points_to?(second, File.join(dotfiles, "shell", ".zshrc"))
      assert_equal ["shell/.zshrc"], parse_map(File.join(dotfiles, MAP_FILE), dotfiles).map(&:repo_rel)
    end
  end

  def test_untrack_directory_by_repo_path
    Dir.mktmpdir do |root|
      dotfiles = File.join(root, "dotfiles")
      source = File.join(root, "home", ".config", "nvim")
      FileUtils.mkdir_p(source)
      File.write(File.join(source, "init.lua"), "settings\n")
      cmd_track(dotfiles, [source, "nvim"])

      cmd_untrack(dotfiles, ["nvim"])

      assert File.directory?(source)
      refute File.symlink?(source)
      assert_equal "settings\n", File.read(File.join(source, "init.lua"))
      refute File.exist?(File.join(dotfiles, "nvim"))
      assert_empty parse_map(File.join(dotfiles, MAP_FILE), dotfiles)
    end
  end

  def test_untrack_ignored_directory_preserves_local_files
    Dir.mktmpdir do |root|
      dotfiles = File.join(root, "dotfiles")
      source = File.join(root, "home", ".config", "nvim")
      FileUtils.mkdir_p(source)
      File.write(File.join(source, "init.lua"), "settings\n")
      File.write(File.join(source, LOCAL_IGNORE), "^/README.*\n")
      cmd_track(dotfiles, [source, "nvim"])
      File.write(File.join(source, "local.txt"), "local\n")

      cmd_untrack(dotfiles, [source])

      assert_equal "settings\n", File.read(File.join(source, "init.lua"))
      refute File.symlink?(File.join(source, "init.lua"))
      assert_equal "local\n", File.read(File.join(source, "local.txt"))
      assert File.exist?(File.join(source, LOCAL_IGNORE))
      refute File.exist?(File.join(dotfiles, "nvim"))
    end
  end

  def test_untrack_refuses_changed_link
    Dir.mktmpdir do |root|
      dotfiles = File.join(root, "dotfiles")
      source = File.join(root, "home", ".bashrc")
      FileUtils.mkdir_p(File.dirname(source))
      File.write(source, "settings\n")
      cmd_track(dotfiles, [source, "shell/.bashrc"])
      File.delete(source)
      File.write(source, "local changes\n")

      assert_raises(RuntimeError) { cmd_untrack(dotfiles, [source]) }

      assert_equal "local changes\n", File.read(source)
      assert_equal "settings\n", File.read(File.join(dotfiles, "shell", ".bashrc"))
      assert_equal 1, parse_map(File.join(dotfiles, MAP_FILE), dotfiles).length
    end
  end

  def test_untrack_refuses_ignored_file_collision
    Dir.mktmpdir do |root|
      dotfiles = File.join(root, "dotfiles")
      source = File.join(root, "home", ".config", "nvim")
      FileUtils.mkdir_p(source)
      File.write(File.join(source, "init.lua"), "settings\n")
      File.write(File.join(source, "README.md"), "in repo\n")
      File.write(File.join(source, LOCAL_IGNORE), "^/README.*\n")
      cmd_track(dotfiles, [source, "nvim"])
      File.write(File.join(source, "README.md"), "local\n")

      assert_raises(RuntimeError) { cmd_untrack(dotfiles, [source]) }

      assert_equal "local\n", File.read(File.join(source, "README.md"))
      assert_equal "in repo\n", File.read(File.join(dotfiles, "nvim", "README.md"))
      assert_equal 1, parse_map(File.join(dotfiles, MAP_FILE), dotfiles).length
    end
  end
end

class TestIntegrationTrackDirectoryAndLink < Minitest::Test
  def test_track_directory_and_link
    Dir.mktmpdir do |root|
      dotfiles_dir = File.join(root, "dotfiles")
      home_dir = File.join(root, "home")
      system_dir = File.join(home_dir, ".config", "nvim")
      nested_system_file = File.join(system_dir, "lua", "plugins.lua")

      FileUtils.mkdir_p(File.dirname(nested_system_file))

      original_content = "return {}\n"
      File.write(nested_system_file, original_content)

      cmd_track(dotfiles_dir, [system_dir, "nvim"])

      repo_dir = File.join(dotfiles_dir, "nvim")
      repo_nested_file = File.join(repo_dir, "lua", "plugins.lua")

      assert_equal original_content, File.read(repo_nested_file)
      assert symlink_points_to?(system_dir, repo_dir)

      assert_equal original_content, File.read(File.join(system_dir, "lua", "plugins.lua"))

      File.delete(system_dir)

      cmd_link(dotfiles_dir)

      assert symlink_points_to?(system_dir, repo_dir)
    end
  end

  def test_track_directory_into_repo_dir
    Dir.mktmpdir do |root|
      dotfiles_dir = File.join(root, "dotfiles")
      home_dir = File.join(root, "home")
      system_dir = File.join(home_dir, ".pi", "agents")
      system_file = File.join(system_dir, "default.md")

      FileUtils.mkdir_p(system_dir)
      File.write(system_file, "pi agent config\n")

      cmd_track(dotfiles_dir, [system_dir, "pi/"])

      repo_dir = File.join(dotfiles_dir, "pi", "agents")
      assert_equal "pi agent config\n", File.read(File.join(repo_dir, "default.md"))
      assert symlink_points_to?(system_dir, repo_dir)

      map_data = File.read(File.join(dotfiles_dir, MAP_FILE))
      assert_includes map_data, "pi/agents : "
    end
  end
end

class TestIntegrationLinkDirectoryWithIgnoreFile < Minitest::Test
  def test_link_directory_with_ignore
    Dir.mktmpdir do |root|
      dotfiles_dir = File.join(root, "dotfiles")
      home_dir = File.join(root, "home")
      system_dir = File.join(home_dir, ".config", "nvim")
      repo_dir = File.join(dotfiles_dir, "nvim")
      repo_init = File.join(repo_dir, "init.lua")
      repo_plugins = File.join(repo_dir, "lua", "plugins.lua")

      FileUtils.mkdir_p(File.dirname(repo_plugins))
      File.write(repo_init, "vim.o.number = true\n")
      File.write(repo_plugins, "return {}\n")
      File.write(File.join(repo_dir, "README.md"), "docs\n")
      File.write(File.join(repo_dir, LOCAL_IGNORE), "^/README.*\n")

      map_path = File.join(dotfiles_dir, MAP_FILE)
      File.write(map_path, "nvim : #{system_dir}\n")

      cmd_link(dotfiles_dir)

      info = File.lstat(system_dir)
      assert info.directory?
      refute info.symlink?

      assert symlink_points_to?(File.join(system_dir, "init.lua"), repo_init)

      nested_info = File.lstat(File.join(system_dir, "lua"))
      assert nested_info.directory?
      refute nested_info.symlink?

      assert symlink_points_to?(File.join(system_dir, "lua", "plugins.lua"), repo_plugins)

      refute File.exist?(File.join(system_dir, "README.md"))
      refute File.exist?(File.join(system_dir, LOCAL_IGNORE))

      status = mapping_status(dotfiles_dir, Mapping.new("nvim", system_dir, system_dir, 1))
      assert_equal "OK", status
    end
  end
end

class TestIntegrationTrackDirectoryWithIgnoreFile < Minitest::Test
  def test_track_directory_with_ignore
    Dir.mktmpdir do |root|
      dotfiles_dir = File.join(root, "dotfiles")
      home_dir = File.join(root, "home")
      system_dir = File.join(home_dir, ".config", "nvim")
      system_init = File.join(system_dir, "init.lua")

      FileUtils.mkdir_p(system_dir)
      File.write(system_init, "vim.o.number = true\n")
      File.write(File.join(system_dir, "README.md"), "docs\n")
      File.write(File.join(system_dir, LOCAL_IGNORE), "^/README.*\n")

      cmd_track(dotfiles_dir, [system_dir, "nvim"])

      repo_dir = File.join(dotfiles_dir, "nvim")
      repo_init = File.join(repo_dir, "init.lua")
      assert File.exist?(File.join(repo_dir, "README.md"))

      info = File.lstat(system_dir)
      assert info.directory?
      refute info.symlink?

      assert symlink_points_to?(File.join(system_dir, "init.lua"), repo_init)

      refute File.exist?(File.join(system_dir, "README.md"))
      refute File.exist?(File.join(system_dir, LOCAL_IGNORE))

      status = mapping_status(dotfiles_dir, Mapping.new("nvim", system_dir, system_dir, 1))
      assert_equal "OK", status
    end
  end
end
