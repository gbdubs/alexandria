// Exercises the app's runtime cache and library-volume monitor without the
// GUI. Built and driven by macos/test/swift-harness.sh; compiled together with
// macos/LibraryVolume.swift and macos/RuntimeCache.swift.
import AppKit
import Foundation

var failures = 0
func check(_ condition: Bool, _ message: String) {
    print(condition ? "ok: \(message)" : "FAIL: \(message)")
    if !condition { failures += 1 }
}

@discardableResult
func run(_ executable: String, _ arguments: String...) -> (status: Int32, output: String) {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: executable)
    process.arguments = arguments
    let pipe = Pipe()
    process.standardOutput = pipe
    process.standardError = pipe
    try! process.run()
    let output = String(decoding: pipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
    process.waitUntilExit()
    return (process.terminationStatus, output.trimmingCharacters(in: .whitespacesAndNewlines))
}

func wait(_ seconds: Double, until condition: () -> Bool) -> Bool {
    let deadline = Date().addingTimeInterval(seconds)
    while Date() < deadline {
        if condition() { return true }
        RunLoop.current.run(until: Date().addingTimeInterval(0.05))
    }
    return condition()
}

func inode(_ url: URL) -> Int? {
    (try? FileManager.default.attributesOfItem(atPath: url.path))?[.systemFileNumber] as? Int
}

/// runtime SUPPORT REAL_APP REAL_APP_V2 REAL_CDHASH STUB_APP STUB_APP_V2 LIBRARY_DIR VOLUME_UUID
func runtimeTests(_ args: [String]) {
    let cache = RuntimeCache(root: URL(fileURLWithPath: args[0], isDirectory: true))
    let real = URL(fileURLWithPath: args[1]), realV2 = URL(fileURLWithPath: args[2])
    let stub = URL(fileURLWithPath: args[4]), stubV2 = URL(fileURLWithPath: args[5])
    let library = URL(fileURLWithPath: args[6], isDirectory: true)
    let fileManager = FileManager.default

    // Build id and signature.
    let signature = try! CodeSignature.of(real)
    check(signature.cdhash == args[3], "build id is the bundle's cdhash (\(signature.cdhash))")
    let signatureV2 = try! CodeSignature.of(realV2)
    check(signatureV2.cdhash != signature.cdhash, "a changed nested pharos changes the build id (\(signatureV2.cdhash))")
    let me = try! CodeSignature.runningCode()
    check(me.cdhash == (try! CodeSignature.of(URL(fileURLWithPath: CommandLine.arguments[0]))).cdhash,
          "running code's cdhash matches its file (\(me.cdhash))")

    // Copy, verify, reuse.
    let copy = try! cache.install(real, signature: signature)
    check(copy.path == cache.runtime.appendingPathComponent("\(signature.cdhash)/Pharos.app").path, "copied to runtime/<cdhash>/Pharos.app")
    check((try? signature.verify(copy)) != nil, "copy passes strict signature validation against the original's requirement")
    check((try? fileManager.destinationOfSymbolicLink(atPath: cache.current.path)) == "\(signature.cdhash)/Pharos.app", "current -> \(signature.cdhash)/Pharos.app")
    check(cache.current.appendingPathComponent("Contents/MacOS/pharos").resolvingSymlinksInPath().path
          == copy.appendingPathComponent("Contents/MacOS/pharos").resolvingSymlinksInPath().path, "current/Contents/MacOS/pharos resolves into the copy")
    let executable = copy.appendingPathComponent("Contents/MacOS/pharos")
    let before = inode(executable)
    _ = try! cache.install(real, signature: signature)
    check(inode(executable) == before, "a valid copy is reused, not copied again")
    check((try? signatureV2.verify(copy)) == nil, "a copy of another build fails verification")

    // Tampering is caught and repaired.
    let icon = copy.appendingPathComponent("Contents/Resources/AppIcon.icns")
    let handle = try! FileHandle(forWritingTo: icon)
    handle.seekToEndOfFile()
    handle.write(Data([0]))
    try! handle.close()
    check((try? signature.verify(copy)) == nil, "a modified copy fails verification")
    _ = try! cache.install(real, signature: signature)
    check((try? signature.verify(copy)) != nil && inode(executable) != before, "install replaces a modified copy")
    do {
        let foreign = try CodeSignature.of(stub)
        _ = try cache.install(real, signature: foreign)
        check(false, "install refuses a bundle that is not the expected build")
    } catch {
        check(true, "install refuses a bundle that is not the expected build: \(error.localizedDescription)")
    }
    check(!(((try? fileManager.contentsOfDirectory(atPath: cache.runtime.path)) ?? []).contains { $0.contains(".partial-") }), "no partial copies left behind")

    // A new build becomes current; the old copy is pruned unless running.
    let copyV2 = try! cache.install(realV2, signature: signatureV2)
    check((try? fileManager.destinationOfSymbolicLink(atPath: cache.current.path)) == "\(signatureV2.cdhash)/Pharos.app", "current follows the newest build")
    let stubSignature = try! CodeSignature.of(stub), stubSignatureV2 = try! CodeSignature.of(stubV2)
    let stubCopy = try! cache.install(stub, signature: stubSignature)
    _ = try! cache.install(stubV2, signature: stubSignatureV2)
    let sleeper = Process()
    sleeper.executableURL = stubCopy.appendingPathComponent("Contents/MacOS/pharos")
    sleeper.arguments = ["--sleep", "30"]
    try! sleeper.run()
    let removed = cache.prune().map(\.lastPathComponent)
    check(removed.contains(signature.cdhash) && removed.contains(signatureV2.cdhash), "prune removes copies that are neither current nor running: \(removed)")
    check(fileManager.fileExists(atPath: stubCopy.path), "prune keeps a copy with a running executable")
    check(fileManager.fileExists(atPath: cache.current.resolvingSymlinksInPath().path), "prune keeps the current copy")
    sleeper.terminate()
    sleeper.waitUntilExit()
    check(cache.prune().map(\.lastPathComponent) == [stubSignature.cdhash], "prune removes that copy once it stops")
    let stale = cache.runtime.appendingPathComponent(".dead.partial-test")
    try! fileManager.createDirectory(at: stale, withIntermediateDirectories: true)
    check(cache.prune().isEmpty, "a fresh partial copy (another launch copying now) is kept")
    check(cache.prune(now: Date().addingTimeInterval(7200)).map(\.lastPathComponent) == [".dead.partial-test"], "an old partial copy is pruned")
    _ = copyV2

    // library.json.
    try! cache.record(library: library)
    let json = (try? JSONSerialization.jsonObject(with: Data(contentsOf: cache.record))) as? [String: Any] ?? [:]
    check(json["library_dir"] as? String == library.standardizedFileURL.path, "library.json library_dir = \(json["library_dir"] ?? "nil")")
    check(json["volume_uuid"] as? String == args[7].uppercased(), "library.json volume_uuid = \(json["volume_uuid"] ?? "nil") (bare)")
    check((json["updated_at"] as? String).map { ISO8601DateFormatter().date(from: $0) != nil } == true, "library.json updated_at is RFC 3339: \(json["updated_at"] ?? "nil")")
    check(Set(json.keys) == ["library_dir", "volume_uuid", "volume_name", "updated_at"], "library.json keys: \(json.keys.sorted())")
    check(cache.readRecord()?.volume?.directory(at: LibraryVolume(containing: library)!.mount).path == library.standardizedFileURL.path,
          "the recorded volume finds the library again")
    let moved = LibraryRecord(libraryDir: "/Volumes/euclid 1/Pharos", volumeUuid: "642C2C39-5926-4831-ABE1-34642F37B103", volumeName: "euclid", updatedAt: Date()).volume
    check(moved?.directory(at: URL(fileURLWithPath: "/Volumes/euclid")).path == "/Volumes/euclid/Pharos" && moved?.name == "euclid",
          "a library recorded under another mount point is found at the volume's next one")

    // Launch decisions.
    let beside = library.appendingPathComponent("Pharos.app")
    check(LaunchPlan.resolve(arguments: ["app"], environment: [:], bundle: beside, cache: cache) == .trampoline(library: library),
          "an app beside library.toml trampolines")
    check(LaunchPlan.resolve(arguments: ["app"], environment: ["PHAROS_RUN_IN_PLACE": "1"], bundle: beside, cache: cache) == .library(library),
          "PHAROS_RUN_IN_PLACE=1 serves in place")
    check(LaunchPlan.resolve(arguments: ["app", "--library", library.path], environment: [:], bundle: cache.current.resolvingSymlinksInPath(), cache: cache)
          == .library(library), "the runtime copy serves the library it is given")
    check(LaunchPlan.resolve(arguments: ["app"], environment: [:], bundle: cache.current.resolvingSymlinksInPath(), cache: cache)
          == .library(URL(fileURLWithPath: library.standardizedFileURL.path, isDirectory: true)), "the runtime copy alone serves the recorded library")
    check(LaunchPlan.resolve(arguments: ["app"], environment: [:], bundle: URL(fileURLWithPath: "/tmp/dist/Pharos.app"), cache: cache) == .user,
          "a dev build without library.toml serves this Mac's configuration")

    // LaunchServices opens the runtime copy with the library (a background-only stub, not Pharos).
    let stubLibrary = URL(fileURLWithPath: args[0]).appendingPathComponent("stub-library", isDirectory: true)
    try! fileManager.createDirectory(at: stubLibrary, withIntermediateDirectories: true)
    let launched = stubLibrary.appendingPathComponent("stub-launched.txt")
    let stubInstalled = try! cache.install(stub, signature: stubSignature)
    var launchError: Error?
    var completed = false
    Trampoline.launch(stubInstalled, library: stubLibrary, cache: cache) { error in
        launchError = error
        completed = true
    }
    check(wait(20) { completed } && launchError == nil, "NSWorkspace opened the runtime copy (\(launchError?.localizedDescription ?? "no error"))")
    check(wait(10) { fileManager.fileExists(atPath: launched.path) }, "the launched copy ran")
    let lines = ((try? String(contentsOf: launched, encoding: .utf8)) ?? "").split(separator: "\n").map(String.init)
    check(lines.first.map { $0.hasPrefix(canonicalPath(stubInstalled) + "/") } == true, "it ran from the runtime copy: \(lines.first ?? "")")
    check(Array(lines.dropFirst()) == ["--library", stubLibrary.path], "it received --library DIR: \(lines.dropFirst())")
    let environment = ((try? String(contentsOf: stubLibrary.appendingPathComponent("stub-env.txt"), encoding: .utf8)) ?? "").split(separator: "\n").map(String.init)
    check(environment.first == "PHAROS_SUPPORT_DIR=\(cache.root.path)" && environment.last.map { $0.count > "HOME=".count } == true,
          "it runs on the same support directory, in an otherwise normal environment: \(environment)")

    // Opening a new build while a copy of another build runs: install has
    // already repointed current and library.json, so the old one must quit.
    let stubV2Installed = try! cache.install(stubV2, signature: stubSignatureV2)
    func open(_ app: URL, _ arguments: [String]) -> NSRunningApplication? {
        let configuration = NSWorkspace.OpenConfiguration()
        configuration.arguments = arguments
        configuration.createsNewApplicationInstance = true
        configuration.activates = false
        var opened: NSRunningApplication?
        var done = false
        NSWorkspace.shared.openApplication(at: app, configuration: configuration) { app, _ in opened = app; done = true }
        _ = wait(20) { done }
        return opened
    }
    func gone(_ app: NSRunningApplication?) -> Bool { app.map { $0.isTerminated || kill($0.processIdentifier, 0) != 0 } ?? true }
    /// Trampoline.launch's completion: nil while pending, .some(nil) on success.
    func trampoline(to library: String, quitTimeout: TimeInterval = 15) -> (URL, Error??) {
        let directory = URL(fileURLWithPath: args[0]).appendingPathComponent(library, isDirectory: true)
        try! fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
        var result: Error?? = nil
        Trampoline.launch(stubV2Installed, library: directory, cache: cache, quitTimeout: quitTimeout) { result = .some($0) }
        _ = wait(quitTimeout + 20) { result != nil }
        return (directory.appendingPathComponent("stub-launched.txt"), result)
    }
    let older = open(stubInstalled, ["--stay"])
    check(!gone(older), "a copy of an older build is running (pid \(older?.processIdentifier ?? 0))")
    let (upgraded, upgrade) = trampoline(to: "upgrade")
    check(upgrade.map { $0 == nil } == true, "the new build opened: \(String(describing: upgrade))")
    check(wait(5) { gone(older) }, "the older copy was asked to quit, and did")
    check(wait(10) { fileManager.fileExists(atPath: upgraded.path) }
          && ((try? String(contentsOf: upgraded, encoding: .utf8)) ?? "").hasPrefix(canonicalPath(stubV2Installed) + "/"), "the new build's copy ran")
    let stubborn = open(stubInstalled, ["--stubborn"])
    let (refused, refusal) = trampoline(to: "refused", quitTimeout: 2)
    check(refusal.map { $0 is Trampoline.OlderCopyRunning } == true, "an older copy that will not quit is reported: \(String(describing: refusal))")
    check(!gone(stubborn) && !fileManager.fileExists(atPath: refused.path), "and the new build is not opened beside it")
    stubborn?.forceTerminate()
    let running = open(stubV2Installed, ["--stay"])
    let (reused, reuse) = trampoline(to: "same")
    check(reuse.map { $0 == nil } == true && !gone(running) && !fileManager.fileExists(atPath: reused.path),
          "a running copy of the same build is brought forward, not opened again")
    running?.forceTerminate()
}

/// service STOPPING_SERVICE_URL
func serviceTests(_ args: [String]) {
    if case .stopping(let body) = ServiceClient.release(URL(string: args[0])!, token: "token") {
        check(body.contains("finishing a write"), "a 503 answer to a release means the service is stopping: \(body)")
    } else {
        check(false, "a 503 answer to a release is not reported as stopping")
    }

    // The app's release state. An eject asks for a release; the service's
    // answer decides the verdict, and later what its exit means.
    let (declined, declinedVerdict) = ReleaseState.after(.failed("HTTP 401: authentication required"))
    check(declined == .none && declinedVerdict != .approve, "a declined release dissents and leaves the library in use")
    check(declined.afterExit(status: 2, libraryPresent: true) == .failed, "a later crash after a declined release is an error, not \"released\"")
    let (finishing, finishingVerdict) = ReleaseState.after(.stopping("{}"))
    check(finishing == .finishing && finishingVerdict == .dissent(ReleaseState.finishingMessage), "a release that ran out of time says Pharos is finishing a write")
    check(finishing.afterExit(status: 0, libraryPresent: true) == .released, "and the library shows as released once the service exits")
    let (released, releasedVerdict) = ReleaseState.after(.released)
    check(released == .released && releasedVerdict == .approve && released.afterExit(status: 0, libraryPresent: true) == .released, "a release lets the eject go ahead")
    check(ReleaseState.after(.notRunning) == (.released, .approve), "with no service there is nothing to release")
    check(released.afterExit(status: 1, libraryPresent: false) == .unplugged && ReleaseState.none.afterExit(status: 1, libraryPresent: false) == .unplugged,
          "a service that exits once its drive is gone shows the library unplugged")
    check(ReleaseState.none.afterExit(status: 0, libraryPresent: true) == .released, "a clean exit closed the catalog: the library can be reopened")

    // The service's stderr is drained as it is written.
    let script = "i=0; while [ $i -lt 2000 ]; do echo 'a line of service diagnostics, 46 bytes long.' >&2; i=$((i+1)); done; echo last-line >&2"
    func shell(_ stderr: Any) -> Process {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sh")
        process.arguments = ["-c", script]
        process.standardError = stderr
        try! process.run()
        return process
    }
    let undrained = Pipe()
    let blocked = shell(undrained)
    check(!wait(3) { !blocked.isRunning }, "control: a process whose stderr nobody reads blocks after 64 KB")
    blocked.terminate()
    let log = ServiceLog(limit: 4096)
    let drained = shell(log.pipe)
    check(wait(10) { !drained.isRunning }, "a service writing 92 KB to a drained stderr runs to the end")
    let text = log.text()
    check(text.hasSuffix("last-line\n") && text.utf8.count <= 4096, "the log keeps the last \(text.utf8.count) bytes, ending \(text.suffix(10).debugDescription)")
    // Its memory stays bounded however much the service writes.
    let before = footprint()
    let flood = ServiceLog()
    let flooding = Process()
    flooding.executableURL = URL(fileURLWithPath: "/bin/sh")
    flooding.arguments = ["-c", "head -c 268435456 /dev/zero >&2; echo end >&2"]
    flooding.standardError = flood.pipe
    try! flooding.run()
    flooding.waitUntilExit()
    let tail = flood.text(waiting: 10)
    let grown = (Int64(footprint()) - Int64(before)) / 1_000_000
    check(tail.hasSuffix("end\n") && tail.utf8.count <= 64 * 1024 && grown < 48, "after 256 MB of stderr the log holds \(tail.utf8.count) bytes and the process grew \(grown) MB")
}

/// This process's memory footprint, as Activity Monitor reports it.
func footprint() -> UInt64 {
    var info = task_vm_info_data_t()
    var count = mach_msg_type_number_t(MemoryLayout<task_vm_info_data_t>.size / MemoryLayout<natural_t>.size)
    let result = withUnsafeMutablePointer(to: &info) {
        $0.withMemoryRebound(to: integer_t.self, capacity: Int(count)) { task_info(mach_task_self_, task_flavor_t(TASK_VM_INFO), $0, &count) }
    }
    return result == KERN_SUCCESS ? info.phys_footprint : 0
}

final class Events {
    private let lock = NSLock()
    private var items: [String] = []
    func add(_ item: String) { lock.withLock { items.append(item) } }
    func all() -> [String] { lock.withLock { items } }
    func count(_ prefix: String) -> Int { all().filter { $0.hasPrefix(prefix) }.count }
}

/// Starts `pharos serve` on a library and waits until it answers.
func serveLibrary(_ pharos: String, config: String, service: URL, token: String) -> Process {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: pharos)
    process.arguments = ["--config", config, "serve"]
    process.standardOutput = FileHandle.nullDevice
    process.standardError = FileHandle(forWritingAtPath: "/dev/null")
    try! process.run()
    let ready = wait(20) {
        var request = URLRequest(url: service.appendingPathComponent("api/health"))
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let done = DispatchSemaphore(value: 0)
        var ok = false
        URLSession.shared.dataTask(with: request) { _, response, _ in
            ok = (response as? HTTPURLResponse)?.statusCode == 200
            done.signal()
        }.resume()
        done.wait()
        return ok
    }
    check(ready, "service is serving the library on the test image")
    return process
}

/// volume UUID MOUNT VOLUME_DEVICE PHAROS CONFIG PORT TOKEN IMAGE_DEVICE
func volumeTests(_ args: [String]) {
    let uuid = args[0], mount = args[1], device = args[2], pharos = args[3], config = args[4]
    let service = URL(string: "http://127.0.0.1:\(args[5])/")!, token = args[6]
    let events = Events()
    var releaseToken = token
    var lastRelease: ServiceClient.Release?

    func startMonitor() -> LibraryVolumeMonitor {
        let monitor = LibraryVolumeMonitor(volumeUUID: uuid)!
        // As the app does it: release, then approve or dissent.
        monitor.approveUnmount = {
            events.add("approval")
            let result = ServiceClient.release(service, token: releaseToken)
            lastRelease = result
            return ReleaseState.after(result).1
        }
        monitor.onUnmount = { events.add("unmount") }
        monitor.onMount = { events.add("mount \($0.path)") }
        monitor.start()
        return monitor
    }
    func serve() -> Process { serveLibrary(pharos, config: config, service: service, token: token) }
    let volume = LibraryVolume(containing: URL(fileURLWithPath: mount).appendingPathComponent("Pharos"))
    check(volume?.uuid == uuid.uppercased() && volume?.relativePath == "Pharos", "LibraryVolume finds the image's UUID and the library within it")

    var monitor = startMonitor()
    check(wait(3) { events.count("mount \(mount)") == 1 }, "monitor reports the mounted volume at start")
    var process = serve()
    // PHAROS_SUPPORT_DIR: the service records its library for the MCP launcher as the app does.
    let record = RuntimeCache.standard.readRecord()
    check(record?.libraryDir == URL(fileURLWithPath: config).deletingLastPathComponent().path && record?.volumeUuid == uuid.uppercased()
          && record?.volumeName == volume?.name, "serve wrote a library.json the app reads back: \(String(describing: record))")

    // A release that fails makes the eject fail with Pharos's message.
    releaseToken = "wrong-token"
    let refused = run("/usr/sbin/diskutil", "unmount", mount)
    check(refused.status != 0 && refused.output.contains("Pharos could not release its library (HTTP 401"), "failed release dissents: \(refused.output.split(separator: "\n").first ?? "")")
    check(events.count("approval") == 1, "approval callback ran once")
    if case .failed(let reason)? = lastRelease { check(reason.contains("401"), "release hook reported the failure (\(reason.prefix(40)))") } else { check(false, "release hook did not fail: \(String(describing: lastRelease))") }
    check(process.isRunning && FileManager.default.fileExists(atPath: config), "service and volume are untouched")

    // A release that succeeds lets the eject through.
    releaseToken = token
    let started = Date()
    let unmounted = run("/usr/sbin/diskutil", "unmount", mount)
    check(unmounted.status == 0, "released volume unmounts (\(String(format: "%.1f", Date().timeIntervalSince(started))) s): \(unmounted.output)")
    check(lastRelease == .released, "release hook released the catalog")
    check(wait(3) { !process.isRunning } && process.terminationStatus == 0, "service exited 0 after the release")
    check(wait(3) { events.count("unmount") >= 1 }, "monitor reports the unmount")

    // The volume comes back.
    let mounted = run("/usr/sbin/diskutil", "mount", device)
    check(mounted.status == 0 && wait(5) { events.count("mount ") >= 2 }, "monitor reports the volume mounting again: \(events.all().last ?? "")")
    let back = events.all().last(where: { $0.hasPrefix("mount ") }).map { String($0.dropFirst(6)) } ?? ""
    check(volume.map { FileManager.default.fileExists(atPath: $0.directory(at: URL(fileURLWithPath: back)).appendingPathComponent("library.toml").path) } == true,
          "the library is found again at \(back)")

    // With no service running there is nothing to release.
    let quiet = run("/usr/sbin/diskutil", "unmount", mount)
    check(quiet.status == 0 && lastRelease == .notRunning, "eject without a running service is approved (\(String(describing: lastRelease)))")
    _ = run("/usr/sbin/diskutil", "mount", device)
    check(wait(5) { events.count("mount ") >= 3 }, "mounted again")

    // Once stopped, the monitor no longer holds up an unmount.
    monitor.stop()
    let approvals = events.count("approval")
    let unwatched = run("/usr/sbin/diskutil", "unmount", mount)
    check(unwatched.status == 0 && events.count("approval") == approvals, "a stopped monitor is not consulted")
    _ = run("/usr/sbin/diskutil", "mount", device)
    monitor = startMonitor()
    check(wait(5) { events.count("mount \(mount)") >= 1 }, "restarted monitor sees the volume")

    // A forced detach, the nearest a test gets to pulling the drive: it may
    // ask for approval but goes ahead whatever the answer. With the release
    // failing, the service still holds the catalog when the volume vanishes,
    // as after a real yank, and must stop by itself.
    process = serve()
    releaseToken = "wrong-token"
    let unmounts = events.count("unmount")
    let yank = run("/usr/bin/hdiutil", "detach", "-force", args[7])
    check(yank.status == 0, "a forced detach goes ahead despite a dissent (approval asked: \(events.count("approval") > approvals))")
    check(wait(5) { events.count("unmount") > unmounts }, "monitor reports the volume gone")
    check(wait(5) { !process.isRunning } && process.terminationStatus == 1, "service exited 1 on its own after its drive vanished (\(process.terminationStatus))")
    monitor.stop()
}

/// eject UUID MOUNT PHAROS CONFIG PORT TOKEN
/// The Eject button's path: release as the app does, then `diskutil eject`.
func ejectTests(_ args: [String]) {
    let uuid = args[0], mount = URL(fileURLWithPath: args[1]), pharos = args[2], config = args[3]
    let service = URL(string: "http://127.0.0.1:\(args[4])/")!, token = args[5]
    let name = mount.lastPathComponent
    let events = Events()

    // The reasons diskutil gives, in words.
    let busy = VolumeEject.describe("Unmount of disk7 failed: at least one volume could not be unmounted\nUnmount was dissented by PID 12473 (/bin/sleep)\nDissenter parent PPID 12380 (/bin/zsh)", volume: "euclid")
    check(busy == "sleep (process 12473) is using euclid, so it could not be ejected. Quit it or close its files on the drive, then eject again.", "a busy drive names the process: \(busy)")
    let spotlight = VolumeEject.describe("Volume euclid on disk7s1 failed to unmount: dissented by PID 99 (/System/Library/Frameworks/CoreServices.framework/Frameworks/Metadata.framework/Support/mds_stores)", volume: "euclid")
    check(spotlight.hasPrefix("Spotlight is using euclid"), "Spotlight is named as Spotlight: \(spotlight)")
    check(VolumeEject.describe("Unable to find disk for /Volumes/euclid\n", volume: "euclid") == "euclid could not be ejected: Unable to find disk for /Volumes/euclid", "any other failure passes diskutil's first line on")

    // The order of the steps.
    var steps: [String] = []
    let kept = LibraryEject.run(release: { steps.append("release"); return .dissent(ReleaseState.finishingMessage) },
                                released: { steps.append("released") }, eject: { steps.append("eject"); return .ejected })
    check(kept == .kept(ReleaseState.finishingMessage) && steps == ["release"], "a refused release keeps the library and never ejects: \(steps)")
    steps = []
    let notEjected = LibraryEject.run(release: { steps.append("release"); return .approve },
                                      released: { steps.append("released") }, eject: { steps.append("eject"); return .failed("busy") })
    check(notEjected == .notEjected("busy") && steps == ["release", "released", "eject"], "the drive is ejected only after the release: \(steps)")

    // For real, on the test image, with the volume monitor approving as the app does.
    var releaseToken = token
    let release: () -> LibraryVolumeMonitor.Verdict = { ReleaseState.after(ServiceClient.release(service, token: releaseToken)).1 }
    let monitor = LibraryVolumeMonitor(volumeUUID: uuid)!
    monitor.approveUnmount = { events.add("approval"); return release() }
    monitor.onUnmount = { events.add("unmount") }
    monitor.onMount = { events.add("mount \($0.path)") }
    monitor.start()
    check(wait(3) { events.count("mount ") >= 1 }, "monitor sees the re-attached image")
    let process = serveLibrary(pharos, config: config, service: service, token: token)

    releaseToken = "wrong-token"
    let refused = LibraryEject.run(release: release, eject: { VolumeEject.eject(mount, name: name) })
    if case .kept(let reason) = refused { check(reason.contains("HTTP 401"), "a refused release keeps the library: \(reason.prefix(70))") } else { check(false, "refused release: \(refused)") }
    check(process.isRunning && FileManager.default.fileExists(atPath: mount.path), "the service and the volume are untouched")

    releaseToken = token
    let held = mount.appendingPathComponent("held-open.txt")
    FileManager.default.createFile(atPath: held.path, contents: Data("busy".utf8))
    let handle = try! FileHandle(forWritingTo: held)
    var releasedFirst = false
    let blocked = LibraryEject.run(release: release, released: { releasedFirst = !process.isRunning || wait(3) { !process.isRunning } },
                                   eject: { VolumeEject.eject(mount, name: name) })
    if case .notEjected(let reason) = blocked {
        check(reason.contains("harness (process \(getpid())) is using \(name)"), "a file held open keeps the drive, and says who: \(reason)")
    } else {
        check(false, "eject with a file held open: \(blocked)")
    }
    check(releasedFirst && process.terminationStatus == 0, "the library was released (service exited \(process.terminationStatus)) before the eject was tried")
    check(FileManager.default.fileExists(atPath: mount.path), "the volume stayed mounted")
    try? handle.close()

    let ejected = LibraryEject.run(release: release, eject: { VolumeEject.eject(mount, name: name) })
    check(ejected == .ejected, "with nothing holding it, the drive ejects: \(ejected)")
    check(wait(5) { events.count("unmount") >= 1 } && !FileManager.default.fileExists(atPath: mount.path), "monitor reports the drive gone and the mount point is gone")
    check(events.count("approval") >= 2, "each eject asked Pharos's monitor first (\(events.count("approval")) approvals)")
    monitor.stop()
}

@main struct Harness {
    static func main() {
        guard ProcessInfo.processInfo.environment["PHAROS_SUPPORT_DIR"] != nil else {
            print("Set PHAROS_SUPPORT_DIR: the services this starts write there.")
            exit(2)
        }
        let args = Array(CommandLine.arguments.dropFirst())
        switch args.first {
        case "runtime": runtimeTests(Array(args.dropFirst()))
        case "volume": volumeTests(Array(args.dropFirst()))
        case "service": serviceTests(Array(args.dropFirst()))
        case "eject": ejectTests(Array(args.dropFirst()))
        default:
            print("usage: harness runtime … | service … | volume … | eject …")
            exit(2)
        }
        print(failures == 0 ? "PASS" : "\(failures) FAILED")
        exit(failures == 0 ? 0 : 1)
    }
}
