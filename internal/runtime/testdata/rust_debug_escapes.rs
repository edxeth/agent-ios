// Oracle for debugQuote (render.go): prints the code point ranges that
// Rust's Debug format for str (agent-browser's hint={:?}) escapes, one
// "first-last" range of lowercase hex per line. Surrogates, which a Rust
// str cannot hold, count as escaped. Regenerate rust_debug_escapes.txt
// with a rustc whose char::UNICODE_VERSION matches Go's unicode.Version:
//
//	rustc -O -o /tmp/rust_debug_escapes rust_debug_escapes.rs && /tmp/rust_debug_escapes > rust_debug_escapes.txt
fn main() {
    let mut start: Option<u32> = None;
    println!("# unicode {:?}", char::UNICODE_VERSION);
    for cp in 0..=0x10FFFFu32 {
        let escaped = match char::from_u32(cp) {
            Some(c) => {
                let s = c.to_string();
                format!("{:?}", s) != format!("\"{}\"", s)
            }
            None => true,
        };
        match (escaped, start) {
            (true, None) => start = Some(cp),
            (false, Some(s)) => {
                println!("{:x}-{:x}", s, cp - 1);
                start = None
            }
            _ => {}
        }
    }
    if let Some(s) = start {
        println!("{:x}-10ffff", s);
    }
}
