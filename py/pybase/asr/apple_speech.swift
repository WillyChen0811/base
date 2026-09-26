// pybase.asr.apple 的辅助程序：macOS 26+ 的 SpeechAnalyzer 只有 Swift API，
// 由 apple.py 首次使用时 swiftc 编译，以子进程调用。
//
// 协议：stdout 每行一个 JSON 对象；出错写一行 {"error": "..."} 并以非 0 退出。
//   locales                        -> {"supported": [...], "installed": [...]}
//   file   <locale> <path>         -> 若干 {"progress": {...}} 与 {"text", "start", "end", "final": true, "words": [...]}
//   stream <locale> <sample_rate> [fast]
//                                  -> 从 stdin 读 s16le 单声道 PCM，EOF 即结束输入；
//                                     除 final 结果外还会有 "final": false 的临时结果。
//                                     fast 开 fastResults：临时结果约 1s 一批（默认约一句一批），
//                                     但临时文本更不准，final 不受影响
// words 是逐词（中文逐字）的 {"text", "start", "end"}，按句切分交给 Python 做。
// progress: {"stage": "installing"|"transcribing", "fraction": 0..1 或 null}——
//   installing 是识别资源缺失时的下载进度（AssetInstallationRequest.progress），
//   transcribing 是 file 模式下按已出结果时间 / 音频总时长估算的进度，stream 模式没有
//   （不知道输入总长）。fraction 为 null 表示暂时算不出（刚开始下载还没有 fractionCompleted）。
// 时间单位整数毫秒。
import AVFoundation
import Foundation
import Speech

func emit(_ obj: [String: Any]) {
    var data = try! JSONSerialization.data(withJSONObject: obj)
    data.append(0x0A)
    FileHandle.standardOutput.write(data)
}

func fail(_ message: String) -> Never {
    emit(["error": message])
    exit(1)
}

func progress(_ stage: String, _ fraction: Double?) {
    var payload: [String: Any] = ["stage": stage]
    if let fraction { payload["fraction"] = fraction }
    emit(["progress": payload])
}

func ms(_ t: CMTime) -> Any {
    let s = t.seconds
    return s.isFinite ? Int((s * 1000).rounded()) : NSNull()
}

func transcriber(_ identifier: String, reporting: Set<SpeechTranscriber.ReportingOption>) async throws -> SpeechTranscriber {
    guard let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: identifier)) else {
        fail("不支持的 locale：\(identifier)")
    }
    let tr = SpeechTranscriber(
        locale: locale,
        transcriptionOptions: [],
        reportingOptions: reporting,
        attributeOptions: [.audioTimeRange]
    )
    // 已装的也会拿到 request，只在确实没装时才提示下载进度；比较要用 BCP 47（Locale 自身的 identifier 是 zh_CN）
    let installed = await SpeechTranscriber.installedLocales.map { $0.identifier(.bcp47) }
    let needsInstall = !installed.contains(locale.identifier(.bcp47))
    if let request = try await AssetInventory.assetInstallationRequest(supporting: [tr]) {
        var observation: NSKeyValueObservation?
        if needsInstall {
            progress("installing", nil)
            observation = request.progress.observe(\.fractionCompleted) { p, _ in
                progress("installing", p.fractionCompleted)
            }
        }
        try await request.downloadAndInstall()
        observation?.invalidate()
    }
    return tr
}

func collect(_ tr: SpeechTranscriber, duration: Double? = nil) async throws {
    for try await r in tr.results {
        emit([
            "text": String(r.text.characters),
            "start": ms(r.range.start),
            "end": ms(r.range.end),
            "final": r.isFinal,
            "words": r.text.runs.map { run -> [String: Any] in
                let range = run.audioTimeRange
                return [
                    "text": String(r.text[run.range].characters),
                    "start": range.map { ms($0.start) } ?? NSNull(),
                    "end": range.map { ms($0.end) } ?? NSNull(),
                ]
            },
        ])
        // stream 模式不知道输入总长，不传 duration，也就不报 transcribing 进度
        if let duration, duration > 0 {
            progress("transcribing", r.range.end.seconds / duration)
        }
    }
}

func runFile(_ identifier: String, _ path: String) async throws {
    let tr = try await transcriber(identifier, reporting: [])
    let analyzer = SpeechAnalyzer(modules: [tr])
    let file: AVAudioFile
    do {
        file = try AVAudioFile(forReading: URL(fileURLWithPath: path))
    } catch {
        // AVAudioFile 读不了 Matroska（webm/mkv）、ogg 等，系统报错只有一串错误码
        fail("读不了音频文件（webm/mkv/ogg 等格式需先转成 wav/m4a）：\(path)")
    }
    let duration = Double(file.length) / file.processingFormat.sampleRate
    async let results: Void = collect(tr, duration: duration)
    if let last = try await analyzer.analyzeSequence(from: file) {
        try await analyzer.finalizeAndFinish(through: last)
    } else {
        await analyzer.cancelAndFinishNow()
    }
    try await results
}

func convert(_ buffer: AVAudioPCMBuffer, _ converter: AVAudioConverter) throws -> AVAudioPCMBuffer {
    let ratio = converter.outputFormat.sampleRate / converter.inputFormat.sampleRate
    let capacity = AVAudioFrameCount(Double(buffer.frameLength) * ratio) + 32
    let out = AVAudioPCMBuffer(pcmFormat: converter.outputFormat, frameCapacity: capacity)!
    var fed = false
    var error: NSError?
    converter.convert(to: out, error: &error) { _, status in
        if fed {
            status.pointee = .noDataNow
            return nil
        }
        fed = true
        status.pointee = .haveData
        return buffer
    }
    if let error { throw error }
    return out
}

func runStream(_ identifier: String, _ sampleRate: Double, fast: Bool) async throws {
    let tr = try await transcriber(identifier, reporting: fast ? [.volatileResults, .fastResults] : [.volatileResults])
    let analyzer = SpeechAnalyzer(modules: [tr])
    let input = AVAudioFormat(commonFormat: .pcmFormatInt16, sampleRate: sampleRate, channels: 1, interleaved: true)!
    let target = await SpeechAnalyzer.bestAvailableAudioFormat(compatibleWith: [tr]) ?? input
    let converter = target == input ? nil : AVAudioConverter(from: input, to: target)
    let (sequence, continuation) = AsyncStream.makeStream(of: AnalyzerInput.self)
    try await analyzer.start(inputSequence: sequence)
    async let results: Void = collect(tr)

    // 阻塞读 stdin 放到独立线程，不占 Swift 并发的协作线程池
    let reader = Thread {
        var pending = Data()
        while true {
            let chunk = FileHandle.standardInput.availableData
            if chunk.isEmpty { break }
            pending.append(chunk)
            let frames = pending.count / 2  // 奇数字节留到下一块，保证按样本对齐
            if frames == 0 { continue }
            let buffer = AVAudioPCMBuffer(pcmFormat: input, frameCapacity: AVAudioFrameCount(frames))!
            buffer.frameLength = AVAudioFrameCount(frames)
            pending.prefix(frames * 2).withUnsafeBytes { raw in
                buffer.int16ChannelData![0].update(
                    from: raw.bindMemory(to: Int16.self).baseAddress!, count: frames)
            }
            pending.removeFirst(frames * 2)
            do {
                continuation.yield(AnalyzerInput(buffer: try converter.map { try convert(buffer, $0) } ?? buffer))
            } catch {
                fail("音频格式转换失败：\(error)")
            }
        }
        continuation.finish()
    }
    reader.start()
    try await analyzer.finalizeAndFinishThroughEndOfInput()
    try await results
}

@main
struct AppleSpeech {
    static func main() async {
        let args = Array(CommandLine.arguments.dropFirst())
        do {
            switch (args.first, args.count) {
            case ("locales", 1):
                emit([
                    "supported": await SpeechTranscriber.supportedLocales.map { $0.identifier(.bcp47) }.sorted(),
                    "installed": await SpeechTranscriber.installedLocales.map { $0.identifier(.bcp47) }.sorted(),
                ])
            case ("file", 3):
                try await runFile(args[1], args[2])
            case ("stream", 3), ("stream", 4):
                guard let rate = Double(args[2]), rate > 0 else { fail("sample_rate 不合法：\(args[2])") }
                guard args.count == 3 || args[3] == "fast" else { fail("未知选项：\(args[3])") }
                try await runStream(args[1], rate, fast: args.count == 4)
            default:
                fail("用法：locales | file <locale> <path> | stream <locale> <sample_rate> [fast]")
            }
        } catch {
            fail("\(error)")
        }
    }
}
