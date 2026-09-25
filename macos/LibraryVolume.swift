import DiskArbitration
import Foundation

/// The volume holding a portable library, as named by the file system.
struct LibraryVolume: Equatable {
    let uuid: String
    let name: String
    let mount: URL
    /// The library directory relative to `mount`, so it can be found again
    /// if the drive comes back at another mount point ("euclid 1").
    let relativePath: String

    /// The removable or external volume holding `directory`, or nil for the
    /// startup disk (which is never ejected) or a volume without a UUID.
    init?(containing directory: URL) {
        let keys: Set<URLResourceKey> = [.volumeUUIDStringKey, .volumeNameKey, .volumeURLKey, .volumeIsRootFileSystemKey, .volumeIsInternalKey]
        guard let values = try? directory.resourceValues(forKeys: keys),
              let uuid = values.volumeUUIDString, let mount = values.volume,
              values.volumeIsRootFileSystem != true, mount.path.hasPrefix("/Volumes/") else { return nil }
        let base = mount.standardizedFileURL.path
        let path = directory.standardizedFileURL.path
        guard path == base || path.hasPrefix(base + "/") else { return nil }
        self.init(uuid: uuid, name: values.volumeName ?? mount.lastPathComponent, mount: mount,
                  relativePath: String(path.dropFirst(base.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/")))
    }

    init(uuid: String, name: String, mount: URL, relativePath: String) {
        self.uuid = uuid.uppercased()
        self.name = name
        self.mount = mount
        self.relativePath = relativePath
    }

    func directory(at mount: URL) -> URL {
        relativePath.isEmpty ? mount : mount.appendingPathComponent(relativePath, isDirectory: true)
    }
}

/// Watches one volume, by UUID, through DiskArbitration: it approves or
/// dissents from polite unmounts (Finder's eject, `diskutil eject`), and
/// reports unmounts, disappearances (a yank or a forced unmount) and mounts.
/// Handlers run on the monitor's serial queue, never the main thread, so
/// `approveUnmount` may block while the library is released.
final class LibraryVolumeMonitor {
    enum Verdict: Equatable {
        case approve
        case dissent(String)
    }

    let volumeUUID: String
    var approveUnmount: () -> Verdict = { .approve }
    var onUnmount: () -> Void = {}
    var onMount: (URL) -> Void = { _ in }

    private let session: DASession
    private let queue: DispatchQueue
    private let match: CFDictionary
    private var started = false

    init?(volumeUUID: String, queue: DispatchQueue = DispatchQueue(label: "Pharos.LibraryVolumeMonitor")) {
        guard let uuid = CFUUIDCreateFromString(kCFAllocatorDefault, volumeUUID as CFString),
              let session = DASessionCreate(kCFAllocatorDefault) else { return nil }
        self.volumeUUID = volumeUUID.uppercased()
        self.session = session
        self.queue = queue
        match = [kDADiskDescriptionVolumeUUIDKey as String: uuid] as CFDictionary
    }

    deinit { stop() }

    /// Registers the callbacks. DiskArbitration reports the volume's current
    /// state straight away: `onMount` if it is mounted.
    func start() {
        guard !started else { return }
        started = true
        let context = Unmanaged.passUnretained(self).toOpaque()
        DARegisterDiskUnmountApprovalCallback(session, match, Self.approval, context)
        DARegisterDiskAppearedCallback(session, match, Self.appeared, context)
        DARegisterDiskDescriptionChangedCallback(session, match, [kDADiskDescriptionVolumePathKey] as CFArray, Self.changed, context)
        DARegisterDiskDisappearedCallback(session, match, Self.disappeared, context)
        DASessionSetDispatchQueue(session, queue)
    }

    /// Unregisters, so DiskArbitration stops waiting on this process for
    /// approval. Handlers may still be running on the queue when it returns.
    func stop() {
        guard started else { return }
        started = false
        let context = Unmanaged.passUnretained(self).toOpaque()
        for callback in [unsafeBitCast(Self.approval, to: UnsafeMutableRawPointer.self),
                         unsafeBitCast(Self.appeared, to: UnsafeMutableRawPointer.self),
                         unsafeBitCast(Self.changed, to: UnsafeMutableRawPointer.self),
                         unsafeBitCast(Self.disappeared, to: UnsafeMutableRawPointer.self)] {
            DAUnregisterCallback(session, callback, context)
        }
        DASessionSetDispatchQueue(session, nil)
    }

    private static func monitor(_ context: UnsafeMutableRawPointer?) -> LibraryVolumeMonitor? {
        context.map { Unmanaged<LibraryVolumeMonitor>.fromOpaque($0).takeUnretainedValue() }
    }

    // Unregistering needs the very function pointers that were registered.
    private static let approval: DADiskUnmountApprovalCallback = { _, context in
        guard case .dissent(let message)? = monitor(context)?.approveUnmount() else { return nil }
        // DiskArbitration releases the dissenter it is handed.
        return Unmanaged.passRetained(DADissenterCreate(kCFAllocatorDefault, DAReturn(kDAReturnBusy), message as CFString))
    }
    private static let appeared: DADiskAppearedCallback = { disk, context in monitor(context)?.report(disk) }
    private static let changed: DADiskDescriptionChangedCallback = { disk, _, context in monitor(context)?.report(disk) }
    private static let disappeared: DADiskDisappearedCallback = { _, context in monitor(context)?.onUnmount() }

    private func report(_ disk: DADisk) {
        let description = DADiskCopyDescription(disk) as? [String: Any]
        if let mount = description?[kDADiskDescriptionVolumePathKey as String] as? URL {
            onMount(mount)
        } else {
            onUnmount()
        }
    }
}

/// Asks a library's service to let go of its catalog (POST /api/release).
enum ServiceClient {
    enum Release: Equatable {
        /// The catalog is closed and the service is exiting.
        case released
        /// Nothing is listening: there is nothing to release.
        case notRunning
        /// The service is stopping but did not close the catalog in time. It
        /// refuses every request while it finishes, then exits.
        case stopping(String)
        case failed(String)
    }

    /// Blocks until the service answers. The service gives up after 7 s,
    /// inside DiskArbitration's ~10 s wait for an eject approval.
    static func release(_ service: URL, token: String, timeout: TimeInterval = 8.5) -> Release {
        var components = URLComponents(url: service, resolvingAgainstBaseURL: false)
        components?.path = "/api/release"
        components?.query = nil
        guard let url = components?.url else { return .failed("invalid service URL \(service)") }
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData, timeoutInterval: timeout)
        request.httpMethod = "POST"
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        let session = URLSession(configuration: .ephemeral)
        defer { session.finishTasksAndInvalidate() }
        let answered = DispatchSemaphore(value: 0)
        var result = Release.failed("no answer")
        let task = session.dataTask(with: request) { data, response, error in
            defer { answered.signal() }
            if let error = error as? URLError, error.code == .cannotConnectToHost {
                result = .notRunning
            } else if let error {
                result = .failed(error.localizedDescription)
            } else if let http = response as? HTTPURLResponse, http.statusCode == 200 {
                result = .released
            } else if let http = response as? HTTPURLResponse, http.statusCode == 503 {
                result = .stopping(String(decoding: data ?? Data(), as: UTF8.self))
            } else {
                let status = (response as? HTTPURLResponse)?.statusCode ?? 0
                result = .failed("HTTP \(status): \(String(decoding: data ?? Data(), as: UTF8.self))")
            }
        }
        task.resume()
        // The handler always runs, if only after a cancel; wait for it.
        if answered.wait(timeout: .now() + timeout + 1) == .timedOut {
            task.cancel()
            answered.wait()
        }
        return result
    }
}

/// A release of the library for an eject, as the app tracks it.
enum ReleaseState: Equatable {
    /// None asked for, or the last was refused and the service carries on.
    case none
    /// Asked for; the service is closing the catalog.
    case asking
    /// The service is finishing a write before it exits (see
    /// ServiceClient.Release.stopping).
    case finishing
    /// The catalog is closed; the service has exited or is about to.
    case released

    static let finishingMessage = "Pharos is finishing a write; try ejecting again in a moment."

    /// The state after the service answered a release, and the verdict for
    /// DiskArbitration.
    static func after(_ answer: ServiceClient.Release) -> (ReleaseState, LibraryVolumeMonitor.Verdict) {
        switch answer {
        case .released, .notRunning: return (.released, .approve)
        case .stopping: return (.finishing, .dissent(finishingMessage))
        case .failed(let reason): return (.none, .dissent("Pharos could not release its library (\(reason)); try ejecting again."))
        }
    }

    enum Exit: Equatable {
        /// The drive went away.
        case unplugged
        /// The catalog was closed cleanly: the library can be reopened.
        case released
        /// The service failed; show why.
        case failed
    }

    /// What the service exiting with `status` means.
    func afterExit(status: Int32, libraryPresent: Bool) -> Exit {
        if !libraryPresent { return .unplugged }
        // The service exits 0 only once it has closed the catalog after a
        // release or SIGTERM, so a clean exit also covers a release whose
        // answer was lost.
        return self != .none || status == 0 ? .released : .failed
    }
}

/// Ejects the drive holding a library, as `diskutil eject` does: every volume
/// on the drive is unmounted, each asking its watchers (Pharos among them)
/// for approval, and the drive is ejected.
enum VolumeEject {
    enum Result: Equatable {
        case ejected
        case failed(String)
    }

    static func eject(_ mount: URL, name: String, timeout: TimeInterval = 60) -> Result {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/sbin/diskutil")
        process.arguments = ["eject", mount.path]
        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe
        let finished = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in finished.signal() }
        do { try process.run() } catch { return .failed("\(name) could not be ejected: \(error.localizedDescription)") }
        // Read while it runs: its output is short, but a full pipe would block it.
        let output = pipe.fileHandleForReading.readDataToEndOfFile()
        if finished.wait(timeout: .now() + timeout) == .timedOut {
            process.terminate()
            return .failed("\(name) did not eject within \(Int(timeout)) seconds. Try again, or eject it in Finder.")
        }
        if process.terminationStatus == 0 { return .ejected }
        return .failed(describe(String(decoding: output, as: UTF8.self), volume: name))
    }

    /// diskutil names the process that kept a volume busy ("Unmount was
    /// dissented by PID 431 (/usr/libexec/thing)"); say it in words.
    static func describe(_ output: String, volume: String) -> String {
        let text = output.trimmingCharacters(in: .whitespacesAndNewlines)
        if let match = text.range(of: #"dissented by PID (\d+) \(([^)]*)\)"#, options: .regularExpression) {
            let found = String(text[match])
            let pid = found.split(separator: " ")[3]
            let path = found.drop(while: { $0 != "(" }).dropFirst().dropLast()
            let process = (path as NSString).lastPathComponent
            if ["mds", "mds_stores", "mdworker", "mdworker_shared", "mdsync"].contains(process) {
                return "Spotlight is using \(volume), so it could not be ejected. Try again in a moment; turning off Spotlight for the drive avoids this (see Settings → Health)."
            }
            let who = process.isEmpty ? "Process \(pid)" : "\(process) (process \(pid))"
            return "\(who) is using \(volume), so it could not be ejected. Quit it or close its files on the drive, then eject again."
        }
        let first = text.split(separator: "\n").first.map(String.init) ?? "diskutil reported no reason"
        return "\(volume) could not be ejected: \(first)"
    }
}

/// The page's Eject button: the library is released first, exactly as for an
/// eject from Finder, and the drive is ejected only once Pharos has let go.
enum LibraryEject {
    enum Outcome: Equatable {
        case ejected
        /// Pharos kept the library: the release was refused, or ran out of
        /// time while the service finishes a write.
        case kept(String)
        /// Pharos let go of the library, but the drive was not ejected.
        case notEjected(String)
    }

    static func run(release: () -> LibraryVolumeMonitor.Verdict, released: () -> Void = {}, eject: () -> VolumeEject.Result) -> Outcome {
        if case .dissent(let reason) = release() { return .kept(reason) }
        released()
        switch eject() {
        case .ejected: return .ejected
        case .failed(let reason): return .notEjected(reason)
        }
    }
}

/// Drains a service's stderr as it is written, keeping the last `limit`
/// bytes for the error view. A pipe nobody reads blocks its writer once
/// its 64 KB buffer fills, which would wedge the service.
final class ServiceLog {
    let pipe = Pipe()
    private let limit: Int
    private let lock = NSLock()
    private var data = Data()
    private let closed = DispatchGroup()

    init(limit: Int = 64 * 1024) {
        self.limit = limit
        closed.enter()
        pipe.fileHandleForReading.readabilityHandler = { [weak self, closed] handle in
            autoreleasepool {
                let chunk = handle.availableData
                guard !chunk.isEmpty else {
                    handle.readabilityHandler = nil
                    closed.leave()
                    return
                }
                self?.append(chunk)
            }
        }
    }

    private func append(_ chunk: Data) {
        lock.withLock {
            data.append(chunk)
            // Data.removeFirst keeps the storage it trims, which then grows
            // with everything ever written, so copy the tail out instead.
            if data.count > 2 * limit { data = Data(data.suffix(limit)) }
        }
    }

    /// The text kept, once the writer has closed the pipe (it exited) or
    /// `timeout` has passed, e.g. while a child it started still holds it.
    func text(waiting timeout: TimeInterval = 1) -> String {
        _ = closed.wait(timeout: .now() + timeout)
        return lock.withLock { String(decoding: data.suffix(limit), as: UTF8.self) }
    }
}
