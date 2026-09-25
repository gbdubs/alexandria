import AppKit
import Darwin
import Foundation
import Security

// Pharos.app on a library's drive runs from a copy on this Mac. Code running
// from the drive keeps it busy, so Finder cannot eject it, and pulling the
// drive kills that code as soon as it pages. The copy lives in the runtime
// cache, keyed by the build's code directory hash (cdhash), which changes
// whenever either executable does: the bundle's seal records the nested
// alexandria's own signature.

/// A simple `key = value` read of Pharos's TOML files, as the app needs only
/// top-level strings and numbers.
func tomlValue(_ name: String, in source: String) -> String? {
    for raw in source.split(separator: "\n") {
        let line = raw.split(separator: "#", maxSplits: 1).first.map(String.init) ?? ""
        guard let separator = line.firstIndex(of: "="),
              line[..<separator].trimmingCharacters(in: .whitespaces) == name else { continue }
        return line[line.index(after: separator)...].trimmingCharacters(in: CharacterSet(charactersIn: " \t\""))
    }
    return nil
}

/// library.json: the library this Mac last opened, for other tools such as
/// the MCP launcher, and for a runtime copy opened while its drive is away.
/// `library_dir` holds library.toml; `volume_uuid` is bare (no "uuid:").
struct LibraryRecord: Codable, Equatable {
    var libraryDir: String
    var volumeUuid: String
    var volumeName: String
    var updatedAt: Date

    /// The volume as it was when this was recorded, for waiting on a drive
    /// that is not connected.
    var volume: LibraryVolume? {
        let parts = URL(fileURLWithPath: libraryDir).pathComponents
        guard !volumeUuid.isEmpty, parts.count >= 3, parts[1] == "Volumes" else { return nil }
        return LibraryVolume(uuid: volumeUuid, name: volumeName.isEmpty ? parts[2] : volumeName,
                             mount: URL(fileURLWithPath: "/Volumes/\(parts[2])", isDirectory: true),
                             relativePath: parts.dropFirst(3).joined(separator: "/"))
    }
}

enum RuntimeCacheError: LocalizedError {
    case unsigned(URL)
    case invalid(URL, OSStatus)
    case mismatch(URL, expected: String, actual: String)

    var errorDescription: String? {
        switch self {
        case .unsigned(let url): return "\(url.path) is not code-signed."
        case .invalid(let url, let status): return "The code signature of \(url.path) is not valid (\(status): \(SecCopyErrorMessageString(status, nil) as String? ?? "unknown"))."
        case .mismatch(let url, let expected, let actual): return "\(url.path) is build \(actual), not \(expected)."
        }
    }
}

/// The identity a runtime copy must have: the same code (cdhash), satisfying
/// the original's designated requirement.
struct CodeSignature {
    let cdhash: String
    let requirement: SecRequirement

    /// The signature of the code running in this process.
    static func runningCode() throws -> CodeSignature {
        var code: SecCode?
        var staticCode: SecStaticCode?
        let status = SecCodeCopySelf([], &code)
        guard status == errSecSuccess, let code else { throw RuntimeCacheError.invalid(Bundle.main.bundleURL, status) }
        let copied = SecCodeCopyStaticCode(code, [], &staticCode)
        guard copied == errSecSuccess, let staticCode else { throw RuntimeCacheError.invalid(Bundle.main.bundleURL, copied) }
        // The dynamic code's cdhash is what the kernel loaded, whatever is on
        // the drive now.
        return try CodeSignature(code: unsafeBitCast(code, to: SecStaticCode.self), requirementOf: staticCode, at: Bundle.main.bundleURL)
    }

    /// The signature of the bundle at `url`, as found on disk.
    static func of(_ url: URL) throws -> CodeSignature {
        let code = try staticCode(url)
        return try CodeSignature(code: code, requirementOf: code, at: url)
    }

    private init(code: SecStaticCode, requirementOf staticCode: SecStaticCode, at url: URL) throws {
        cdhash = try Self.cdhash(code, at: url)
        var requirement: SecRequirement?
        let status = SecCodeCopyDesignatedRequirement(staticCode, [], &requirement)
        guard status == errSecSuccess, let requirement else { throw RuntimeCacheError.invalid(url, status) }
        self.requirement = requirement
    }

    static func staticCode(_ url: URL) throws -> SecStaticCode {
        var code: SecStaticCode?
        let status = SecStaticCodeCreateWithPath(url as CFURL, [], &code)
        guard status == errSecSuccess, let code else { throw RuntimeCacheError.invalid(url, status) }
        return code
    }

    static func cdhash(_ code: SecStaticCode, at url: URL) throws -> String {
        var information: CFDictionary?
        let status = SecCodeCopySigningInformation(code, [], &information)
        guard status == errSecSuccess else { throw RuntimeCacheError.invalid(url, status) }
        guard let unique = (information as? [String: Any])?[kSecCodeInfoUnique as String] as? Data else { throw RuntimeCacheError.unsigned(url) }
        return unique.map { String(format: "%02x", $0) }.joined()
    }

    /// Throws unless the bundle at `url` is intact, fully validated (every
    /// architecture and nested code), and exactly this code.
    func verify(_ url: URL) throws {
        let code = try Self.staticCode(url)
        let flags = SecCSFlags(rawValue: kSecCSCheckAllArchitectures | kSecCSCheckNestedCode | kSecCSStrictValidate)
        let status = SecStaticCodeCheckValidity(code, flags, requirement)
        guard status == errSecSuccess else { throw RuntimeCacheError.invalid(url, status) }
        let actual = try Self.cdhash(code, at: url)
        guard actual == cdhash else { throw RuntimeCacheError.mismatch(url, expected: cdhash, actual: actual) }
    }
}

struct RuntimeCache {
    /// ~/Library/Application Support/Pharos, or $PHAROS_SUPPORT_DIR.
    let root: URL
    var runtime: URL { root.appendingPathComponent("runtime", isDirectory: true) }
    /// Always points at the copy last opened from a library, e.g. for
    /// current/Contents/MacOS/alexandria.
    var current: URL { runtime.appendingPathComponent("current") }
    var record: URL { root.appendingPathComponent("library.json") }

    static let standard = RuntimeCache(root: ProcessInfo.processInfo.environment["PHAROS_SUPPORT_DIR"].map { URL(fileURLWithPath: $0, isDirectory: true) }
        ?? FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/Pharos", isDirectory: true))

    /// Whether `bundle` is one of this cache's copies.
    func contains(_ bundle: URL) -> Bool {
        canonicalPath(bundle).hasPrefix(canonicalPath(runtime) + "/")
    }

    /// Returns a verified copy of `bundle` in the cache, reusing one that is
    /// already there, and points `current` at it.
    func install(_ bundle: URL, signature: CodeSignature) throws -> URL {
        let fileManager = FileManager.default
        try fileManager.createDirectory(at: runtime, withIntermediateDirectories: true)
        let build = runtime.appendingPathComponent(signature.cdhash, isDirectory: true)
        let copy = build.appendingPathComponent(bundle.lastPathComponent, isDirectory: true)
        if (try? signature.verify(copy)) == nil {
            // Copy beside the cache and move into place, so a copy that is
            // interrupted or races another launch is never used half-made.
            let partial = runtime.appendingPathComponent(".\(signature.cdhash).partial-\(UUID().uuidString)", isDirectory: true)
            defer { try? fileManager.removeItem(at: partial) }
            try fileManager.createDirectory(at: partial, withIntermediateDirectories: false)
            try fileManager.copyItem(at: bundle, to: partial.appendingPathComponent(bundle.lastPathComponent, isDirectory: true))
            try signature.verify(partial.appendingPathComponent(bundle.lastPathComponent, isDirectory: true))
            // Another launch may have put a valid copy in place meanwhile.
            if (try? signature.verify(copy)) == nil {
                try? fileManager.removeItem(at: build)
                guard rename(partial.path, build.path) == 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
            }
        }
        try point(current, at: "\(signature.cdhash)/\(bundle.lastPathComponent)")
        return copy
    }

    private func point(_ link: URL, at destination: String) throws {
        let temporary = runtime.appendingPathComponent(".current-\(UUID().uuidString)")
        try FileManager.default.createSymbolicLink(atPath: temporary.path, withDestinationPath: destination)
        guard rename(temporary.path, link.path) == 0 else {
            let error = POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            try? FileManager.default.removeItem(at: temporary)
            throw error
        }
    }

    /// Removes copies that are neither current nor running (the app, its
    /// service, or an MCP server started from one), and stale partial copies.
    @discardableResult
    func prune(running: [String] = runningExecutables(), now: Date = Date()) -> [URL] {
        let fileManager = FileManager.default
        let keep = (try? fileManager.destinationOfSymbolicLink(atPath: current.path))?.split(separator: "/").first.map(String.init)
        let entries = (try? fileManager.contentsOfDirectory(at: runtime, includingPropertiesForKeys: [.contentModificationDateKey, .isSymbolicLinkKey])) ?? []
        var removed: [URL] = []
        for entry in entries {
            let name = entry.lastPathComponent
            let values = try? entry.resourceValues(forKeys: [.contentModificationDateKey, .isSymbolicLinkKey])
            if name == "current" || name == keep || values?.isSymbolicLink == true { continue }
            if name.hasPrefix(".") {
                guard now.timeIntervalSince(values?.contentModificationDate ?? now) > 3600 else { continue }
            } else {
                let prefix = canonicalPath(entry) + "/"
                guard !running.contains(where: { $0.hasPrefix(prefix) }) else { continue }
            }
            if (try? fileManager.removeItem(at: entry)) != nil { removed.append(entry) }
        }
        return removed
    }

    func write(_ library: LibraryRecord) throws {
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        encoder.dateEncodingStrategy = .iso8601
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try encoder.encode(library).write(to: record, options: .atomic)
    }

    func readRecord() -> LibraryRecord? {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        decoder.dateDecodingStrategy = .iso8601
        return (try? Data(contentsOf: record)).flatMap { try? decoder.decode(LibraryRecord.self, from: $0) }
    }

    /// Records the library in `directory` (holding library.toml). Its volume
    /// is the one library.toml pins, else the one it is on now.
    func record(library directory: URL, now: Date = Date()) throws {
        let config = directory.appendingPathComponent("library.toml")
        let pin = (try? String(contentsOf: config, encoding: .utf8)).flatMap { tomlValue("volume_id", in: $0) } ?? ""
        let volume = LibraryVolume(containing: directory)
        let uuid = pin.hasPrefix("uuid:") ? String(pin.dropFirst(5)).uppercased() : volume?.uuid ?? ""
        try write(LibraryRecord(libraryDir: directory.standardizedFileURL.path, volumeUuid: uuid,
                                volumeName: volume?.name ?? "", updatedAt: now))
    }
}

/// The path with every symlink resolved, as the kernel reports executables.
/// (URL.resolvingSymlinksInPath drops /private from /private/var paths.)
func canonicalPath(_ url: URL) -> String {
    guard let resolved = realpath(url.path, nil) else { return url.standardizedFileURL.path }
    defer { free(resolved) }
    return String(cString: resolved)
}

/// Paths of every executable this user is running.
func runningExecutables() -> [String] {
    let count = proc_listallpids(nil, 0)
    guard count > 0 else { return [] }
    var pids = [pid_t](repeating: 0, count: Int(count) + 64)
    let filled = pids.withUnsafeMutableBytes { proc_listallpids($0.baseAddress, Int32($0.count)) }
    var path = [CChar](repeating: 0, count: 4 * Int(MAXPATHLEN))
    return pids.prefix(Int(max(filled, 0))).compactMap { pid in
        proc_pidpath(pid, &path, UInt32(path.count)) > 0 ? String(cString: path) : nil
    }
}

/// How this launch of Pharos.app should proceed.
enum LaunchPlan: Equatable {
    /// Copy this bundle to the runtime cache, open the copy there, and quit.
    case trampoline(library: URL)
    /// Serve the library in this directory.
    case library(URL)
    /// Serve this Mac's own configuration.
    case user

    static func resolve(arguments: [String], environment: [String: String], bundle: URL, cache: RuntimeCache) -> LaunchPlan {
        if let index = arguments.firstIndex(of: "--library"), index + 1 < arguments.count {
            return .library(URL(fileURLWithPath: arguments[index + 1], isDirectory: true))
        }
        let beside = bundle.deletingLastPathComponent()
        if FileManager.default.fileExists(atPath: beside.appendingPathComponent("library.toml").path) {
            // PHAROS_RUN_IN_PLACE=1 runs from the drive, e.g. to debug this.
            return environment["PHAROS_RUN_IN_PLACE"] == "1" || cache.contains(bundle) ? .library(beside) : .trampoline(library: beside)
        }
        // A runtime copy opened without arguments (from the Dock, say)
        // serves the library last opened on this Mac.
        if cache.contains(bundle), let record = cache.readRecord() {
            return .library(URL(fileURLWithPath: record.libraryDir, isDirectory: true))
        }
        return .user
    }
}

enum Trampoline {
    /// Prepares the runtime copy of the running app for the library in
    /// `directory`. Blocks for the copy; call it off the main thread.
    static func prepare(library directory: URL, cache: RuntimeCache = .standard) throws -> URL {
        let copy = try cache.install(Bundle.main.bundleURL, signature: CodeSignature.runningCode())
        try cache.record(library: directory)
        cache.prune()
        return copy
    }

    /// Opens `copy` for the library, or brings it forward if it is already
    /// running (Pharos keeps one window per Mac). A running copy of another
    /// build is asked to quit first, since `current` and library.json now name
    /// this one; if it has not quit within `quitTimeout`, completes with
    /// `Trampoline.OlderCopyRunning`. Completes on any queue.
    static func launch(_ copy: URL, library directory: URL, cache: RuntimeCache = .standard, quitTimeout: TimeInterval = 15,
                       completion: @escaping (Error?) -> Void) {
        let identifier = Bundle(url: copy)?.bundleIdentifier ?? ""
        let running = NSRunningApplication.runningApplications(withBundleIdentifier: identifier)
            .filter { $0.bundleURL.map(cache.contains) == true && $0 != NSRunningApplication.current }
        if let same = running.first(where: { $0.bundleURL.map(canonicalPath) == canonicalPath(copy) }) {
            same.activate()
            completion(nil)
            return
        }
        let open = {
            let configuration = NSWorkspace.OpenConfiguration()
            configuration.arguments = ["--library", directory.path]
            // LaunchServices starts the copy with its own environment; keep
            // it (and its service) on this cache.
            configuration.environment = ["PHAROS_SUPPORT_DIR": cache.root.path]
            configuration.createsNewApplicationInstance = true
            configuration.activates = true
            NSWorkspace.shared.openApplication(at: copy, configuration: configuration) { _, error in completion(error) }
        }
        guard !running.isEmpty else { return open() }
        // Quitting stops its service, which the new copy then waits out.
        running.forEach { $0.terminate() }
        DispatchQueue.global(qos: .userInitiated).async {
            let exited = { (app: NSRunningApplication) in app.isTerminated || kill(app.processIdentifier, 0) != 0 }
            let deadline = Date().addingTimeInterval(quitTimeout)
            while !running.allSatisfy(exited), Date() < deadline { Thread.sleep(forTimeInterval: 0.1) }
            if let stuck = running.first(where: { !exited($0) }) {
                completion(OlderCopyRunning(bundle: stuck.bundleURL ?? copy, pid: stuck.processIdentifier))
            } else {
                open()
            }
        }
    }

    struct OlderCopyRunning: LocalizedError {
        let bundle: URL
        let pid: pid_t
        var errorDescription: String? {
            "Another build of Pharos is still running (\(bundle.path), process \(pid)) and did not quit. Quit it, then open Pharos again."
        }
    }
}
